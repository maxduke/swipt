package device

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	"github.com/swipt-automation/swipt/internal/coredevice"
	"github.com/swipt-automation/swipt/internal/coredevice/imagemounter"
	"github.com/swipt-automation/swipt/internal/version"
)

var errRemounted = E.New("the Developer Disk Image is mounted now; reconnecting to reach its services")

// The Developer Disk Image has a Cryptex identity without ApBoardID or ApChipID; its personalized identities include both
// (DDI 27A9266e, BuildManifest.plist, BuildIdentities 0..139 and 140).
var diskImageVariants = []imagemounter.Variant{imagemounter.Cryptex, imagemounter.Personalized}

type diskImages struct {
	directory string
	logger    logger.ContextLogger
	relisted  bool
	replaced  bool
}

func PrepareDiskImage(ctx context.Context, connect func(ctx context.Context) (*coredevice.Device, error), directory string, logger logger.ContextLogger) error {
	images := diskImages{directory: directory, logger: logger}
	var err error
	for range 4 {
		var device *coredevice.Device
		device, err = connect(ctx)
		if err != nil {
			return err
		}
		err = images.prepare(ctx, device)
		device.Close()
		if !errors.Is(err, errRemounted) {
			return err
		}
	}
	return err
}

func (d *diskImages) prepare(ctx context.Context, device *coredevice.Device) error {
	images := imagemounter.New(device, d.logger)
	current, knownVersion := version.Parse(device.Info().OSVersion)
	// DDI cryptex asset indices changed in iOS 26.4 (pymobiledevice3 cae0191,
	// services/cryptexd.py, DDI_CRYPTEX_MIN_VERSION).
	cryptexSupported := !knownVersion || current.AtLeast(version.Version{26, 4})
	variants := []imagemounter.Variant{imagemounter.Personalized}
	if cryptexSupported {
		variants = diskImageVariants
	}
	status, err := images.Status(ctx)
	if err != nil {
		return E.Cause(err, "read the Developer Disk Image's state")
	}
	if status.Mounted() {
		if status.CryptexError == nil {
			if err = d.update(ctx, images, status, variants[0]); err != nil {
				return err
			}
		}
		if device.HasService(displayService) {
			d.relisted = false
			return nil
		}
		if !d.relisted {
			d.relisted = true
			return errRemounted
		}
		return nil
	}
	if !status.DeveloperMode {
		return newError(ProblemDeveloperModeOff, "Developer Mode is off; turn it on in Settings › Privacy & Security › Developer Mode")
	}
	directory := d.directory
	if directory == "" {
		return newError(ProblemNoDiskImage, "the Developer Disk Image is not mounted, and there is no directory for its files")
	}
	var failure error
	if cryptexSupported && status.CryptexError != nil {
		d.logger.Warn("cryptexd failed, trying the personalized disk image first: ", status.CryptexError)
		failure = E.Cause(status.CryptexError, "list installed cryptexes")
		variants = []imagemounter.Variant{imagemounter.Personalized, imagemounter.Cryptex}
	}
	found, hasCopy := false, false
	for _, variant := range variants {
		files, err := d.diskImageFiles(ctx, directory, variant)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			d.logger.Info(E.Cause(err, "find ", variant, " disk image files"))
			failure = E.Errors(failure, err)
			hasCopy = hasCopy || !errors.Is(err, fs.ErrNotExist)
			continue
		}
		found = true
		d.logger.Info("mounting the ", variant, " Developer Disk Image from ", files)
		if variant == imagemounter.Cryptex {
			err = images.MountCryptex(ctx, files)
		} else {
			err = images.MountPersonalized(ctx, files)
		}
		switch {
		case err == nil, errors.Is(err, imagemounter.ErrAlreadyMounted):
			d.logger.Info("mounted the ", variant, " Developer Disk Image")
			d.relisted = true
			return errRemounted
		case errors.Is(err, imagemounter.ErrDeveloperModeOff):
			return &Error{Problem: ProblemDeveloperModeOff, Err: err}
		case ctx.Err() != nil:
			return ctx.Err()
		}
		d.logger.Warn(E.Cause(err, "mount the ", variant, " Developer Disk Image"))
		failure = E.Errors(failure, E.Cause(err, "mount ", variant))
	}
	if !found {
		if !hasCopy {
			return newError(ProblemNoDiskImage, "the Developer Disk Image is not mounted, and there is no copy of it to mount")
		}
		return &Error{Problem: ProblemNoDiskImage, Err: E.Cause(failure, "the Developer Disk Image is not mounted, and ", directory, " has no usable copy")}
	}
	return &Error{Problem: ProblemDiskImage, Err: failure}
}

func (d *diskImages) update(ctx context.Context, images *imagemounter.Client, status imagemounter.Status, preferred imagemounter.Variant) error {
	if d.directory == "" || status.Build == "" {
		return nil
	}
	variant := preferred
	if status.Cryptex {
		variant = imagemounter.Cryptex
	}
	files, err := d.diskImageFiles(ctx, d.directory, variant)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d.logger.Info(E.Cause(err, "find ", variant, " disk image files to replace the Developer Disk Image ", status.Build))
		return nil
	}
	available, outdated, err := imagemounter.Outdated(ctx, status.Build, files)
	if err != nil {
		return &Error{Problem: ProblemDiskImage, Err: E.Cause(err, "compare the Developer Disk Image's version")}
	}
	if !outdated {
		d.replaced = false
		return nil
	}
	if d.replaced {
		d.logger.Warn("the Developer Disk Image ", status.Build, " is still older than ", available, " after replacing it")
		return nil
	}
	d.logger.Info("replacing the Developer Disk Image ", status.Build, " with ", available, " from ", files)
	if variant == imagemounter.Cryptex {
		err = images.ReplaceCryptex(ctx, files, !status.Cryptex)
	} else {
		err = images.Unmount(ctx)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{Problem: ProblemDiskImage, Err: E.Cause(err, "replace the Developer Disk Image ", status.Build, " with ", available)}
	}
	d.replaced = true
	d.relisted = true
	return errRemounted
}

func (d *diskImages) diskImageFiles(ctx context.Context, directory string, variant imagemounter.Variant) (string, error) {
	if imagemounter.Verify(ctx, directory, variant) == nil {
		return directory, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	own := filepath.Join(directory, variant.String())
	err := imagemounter.Verify(ctx, own, variant)
	if err == nil {
		return own, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", E.Cause(err, "verify ", variant)
}
