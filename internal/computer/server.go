//go:build darwin && cgo

package computer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/api"
	"github.com/swipt-automation/swipt/internal/api/discovery"
	"github.com/swipt-automation/swipt/internal/automation"
	"github.com/swipt-automation/swipt/internal/device"
	"github.com/swipt-automation/swipt/internal/dnssd"
	"github.com/swipt-automation/swipt/internal/graphics"
	"github.com/swipt-automation/swipt/internal/log"

	"golang.org/x/sys/unix"
)

const (
	lockFile       = "server.lock"
	swipletsFolder = "Swiplets"
	diskImages     = "/Library/Developer/DeveloperDiskImages/iOS_DDI/Restore"
	shutdownGrace  = 2 * time.Second
)

type Options struct {
	Device       string
	Passcode     string
	PasscodeType string
	DiskImage    string
	Version      string
	Logs         *log.Factory
	Output       io.Writer
}

func Run(ctx context.Context, options Options) error {
	passcodeKind, err := passcodeType(options.PasscodeType)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	directory := filepath.Join(home, "Library/Application Support/Swipt")
	caches := filepath.Join(home, "Library/Caches/Swipt")
	if err = os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	stored, err := loadSettings(directory)
	if err != nil {
		return err
	}
	host := LocalHostName() + ".local"
	url := func() string {
		return "http://" + net.JoinHostPort(host, strconv.Itoa(stored.Port))
	}
	held, err := acquire(filepath.Join(directory, lockFile), url())
	if err != nil {
		return err
	}
	defer held.Close()
	paired, err := chooseDevice(ctx, options.Device)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(stored.Port))
	if errors.Is(err, syscall.EADDRINUSE) {
		stored.Port = randomPort()
		if err = stored.save(directory); err != nil {
			return err
		}
		listener, err = net.Listen("tcp", ":"+strconv.Itoa(stored.Port))
	}
	if err != nil {
		return E.Cause(err, "listen on port ", stored.Port)
	}
	defer listener.Close()

	deviceLogger := options.Logs.NewLogger("device")
	connection := device.New(device.Options{
		Name:               paired.Name,
		Connect:            connect(paired.UDID, deviceLogger),
		Logger:             deviceLogger,
		Observer:           observer{},
		DiskImageDirectory: diskImageDirectory(options.DiskImage),
	})
	if options.Passcode != "" {
		if err = connection.UpdateSettings(device.Settings{AutoUnlockPasscode: options.Passcode, AutoUnlockPasscodeType: passcodeKind}); err != nil {
			return err
		}
	}
	if err = connection.Start(); err != nil {
		return err
	}
	defer connection.Close()
	osVersion, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return E.Cause(err, "read the macOS version")
	}
	swiplets := automation.New(automation.Options{
		Device:     connection.Phone(),
		Directory:  filepath.Join(directory, swipletsFolder),
		Caches:     filepath.Join(caches, swipletsFolder),
		Languages:  Languages(),
		OSVersion:  osVersion,
		LogFactory: options.Logs,
		Graphics:   graphics.New(graphics.Options{Connection: connection, Logger: options.Logs.NewLogger("graphics")}),
	})
	swiplets.Start()
	defer swiplets.Close()

	stop := make(chan struct{})
	var stopOnce sync.Once
	server := api.New(api.Options{
		Device: connection, Swiplets: swiplets, Logs: options.Logs, Logger: options.Logs.NewLogger("api"),
		AppVersion: options.Version, AppBuild: "macOS",
		Shutdown: func() { stopOnce.Do(func() { close(stop) }) },
	})
	advertisement, err := dnssd.Advertise(ctx, Name(), discovery.ServiceType, uint16(stored.Port), nil)
	if err != nil {
		return err
	}
	defer advertisement.Close()
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener, "", nil) }()
	fmt.Fprintln(options.Output, "Device:", paired.Name, "("+paired.UDID+")")
	fmt.Fprintln(options.Output, "Swipt API:", url())
	fmt.Fprintln(options.Output, "MCP:", url()+api.MCPPath)
	select {
	case <-ctx.Done():
	case <-stop:
	case err = <-served:
		if err != nil {
			return E.Cause(err, "serve the API")
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err = server.Shutdown(shutdownCtx); err != nil {
		options.Logs.NewLogger("api").Debug("stopped: ", err)
	}
	return nil
}

type MountOptions struct {
	Device    string
	DiskImage string
	Logs      *log.Factory
}

func Mount(ctx context.Context, options MountOptions) error {
	paired, err := chooseDevice(ctx, options.Device)
	if err != nil {
		return err
	}
	logger := options.Logs.NewLogger("device")
	return device.PrepareDiskImage(ctx, connect(paired.UDID, logger), diskImageDirectory(options.DiskImage), logger)
}

func diskImageDirectory(given string) string {
	if given != "" {
		return given
	}
	if _, err := os.Stat(diskImages); err != nil {
		return ""
	}
	return diskImages
}

func passcodeType(name string) (device.PasscodeType, error) {
	switch name {
	case "", "pin":
		return device.PasscodePIN, nil
	case "numeric":
		return device.PasscodeNumeric, nil
	case "alphanumeric":
		return device.PasscodeAlphanumeric, nil
	default:
		return 0, E.New("unknown passcode type ", name, "; it is pin, numeric or alphanumeric")
	}
}
