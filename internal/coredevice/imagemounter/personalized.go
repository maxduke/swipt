// The personalized mount follows go-ios ios/imagemounter
// (https://github.com/danielpaulus/go-ios, MIT License, Copyright (c) 2019
// danielpaulus).

package imagemounter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice/rsd"
	"github.com/swipt-automation/swipt/internal/coredevice/xpc"
)

// pymobiledevice3 queries the device for an existing personalization
// manifest before requesting a ticket from TSS (pymobiledevice3 9c33842,
// pymobiledevice3/services/mobile_image_mounter.py:396-406).
func (c *Client) MountPersonalized(ctx context.Context, dir string) error {
	manifest, err := loadManifest(ctx, dir)
	if err != nil {
		return err
	}
	if err = c.checkCanMount(ctx); err != nil {
		return err
	}
	var (
		identifiers map[string]any
		identity    buildIdentity
		image       resolvedFile
		trustCache  []byte
		ticket      []byte
	)
	err = c.withMounter(ctx, func(conn *mounterConn) error {
		var err error
		if identifiers, err = conn.personalizationIdentifiers(); err != nil {
			return err
		}
		board, _ := numberValue(identifiers["BoardId"])
		chip, _ := numberValue(identifiers["ChipID"])
		if identity, err = manifest.personalizedIdentity(board, chip); err != nil {
			return err
		}
		if image, err = identity.resolve(dir, "PersonalizedDMG"); err != nil {
			return err
		}
		if trustCache, err = identity.readFile(ctx, dir, "LoadableTrustCache"); err != nil {
			return err
		}
		if err = image.verify(ctx); err != nil {
			return err
		}
		// pymobiledevice3 uses the image SHA-384 for QueryPersonalizationManifest
		// (pymobiledevice3 9c33842,
		// pymobiledevice3/services/mobile_image_mounter.py:401-403).
		ticket, err = conn.personalizationManifest(image.digest)
		return err
	})
	if err != nil {
		return err
	}
	// go-ios and pymobiledevice3 query the nonce on the connection used to
	// upload and mount the image (go-ios ca2c56d,
	// ios/imagemounter/personalized_image_mounter.go:88-140; pymobiledevice3
	// 9c33842, pymobiledevice3/services/mobile_image_mounter.py:404-414,432).
	return c.withMounter(ctx, func(conn *mounterConn) error {
		if ticket == nil {
			c.logger.Info("requesting a Developer Disk Image ticket from Apple")
			var err error
			if ticket, err = c.personalizedTicket(ctx, conn, identity, identifiers); err != nil {
				return err
			}
		}
		if err := conn.upload(ctx, image, ticket); err != nil {
			return err
		}
		if err := conn.mount(ticket, trustCache); err != nil {
			return err
		}
		c.logger.Info("mounted the personalized Developer Disk Image")
		return conn.hangUp()
	})
}

func (c *Client) Unmount(ctx context.Context) error {
	return c.withMounter(ctx, func(conn *mounterConn) error {
		const command = "UnmountImage"
		reply, err := conn.request(map[string]any{"Command": command, "MountPath": developerMountPath})
		if err != nil {
			return err
		}
		return conn.expectStatus(command, reply, "Complete")
	})
}

func (c *Client) personalizedTicket(ctx context.Context, conn *mounterConn, identity buildIdentity, identifiers map[string]any) ([]byte, error) {
	ecid, err := c.ecid(ctx)
	if err != nil {
		return nil, err
	}
	nonce, err := conn.nonce()
	if err != nil {
		return nil, err
	}
	request, err := personalizedRequest(identity, identifiers, ecid, nonce)
	if err != nil {
		return nil, err
	}
	response, err := sendTSS(ctx, request)
	if err != nil {
		return nil, err
	}
	ticket, ok := response["ApImg4Ticket"].([]byte)
	if !ok {
		return nil, E.New("imagemounter: TSS response without ApImg4Ticket")
	}
	return ticket, nil
}

func (c *Client) withMounter(ctx context.Context, fn func(conn *mounterConn) error) error {
	mounter := new(mounterConn)
	var (
		conn io.Closer
		err  error
	)
	if c.device.HasService(mounterService) || !c.device.HasService(mounterBridgeService) {
		mounter.conn, err = c.device.DialShim(ctx, mounterService)
		conn = mounter.conn
	} else {
		mounter.bridge, err = c.device.DialXPC(ctx, mounterBridgeService)
		conn = mounter.bridge
	}
	if err != nil {
		return E.Cause(err, "imagemounter")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	err = fn(mounter)
	if !stop() && err != nil && ctx.Err() != nil {
		return E.Cause(ctx.Err(), "imagemounter: image mounter")
	}
	return err
}

type mounterConn struct {
	conn   *rsd.PlistConn
	bridge *xpc.Conn
}

func (m *mounterConn) request(message map[string]any) (map[string]any, error) {
	command, _ := message["Command"].(string)
	var err error
	if m.bridge != nil {
		if _, found := message["HostProcessName"]; !found {
			message["HostProcessName"] = "swipt"
		}
		err = m.bridge.Send(map[string]any{"XPCRequestDictionary": message}, true)
	} else {
		err = m.conn.Send(message)
	}
	if err != nil {
		return nil, E.Cause(err, "imagemounter: send ", command)
	}
	return m.receive(command)
}

func (m *mounterConn) receive(command string) (map[string]any, error) {
	var (
		reply map[string]any
		err   error
	)
	if m.bridge != nil {
		reply, err = m.bridge.Receive()
	} else {
		reply, err = m.conn.Receive()
	}
	if err != nil {
		return nil, E.Cause(err, "imagemounter: receive ", command, " reply")
	}
	if message, ok := reply["Error"]; ok {
		detail, _ := reply["DetailedError"].(string)
		return nil, &DeviceError{Command: command, Message: fmt.Sprint(message), Detail: detail}
	}
	return reply, nil
}

func (m *mounterConn) expectStatus(command string, reply map[string]any, want string) error {
	if status, _ := reply["Status"].(string); status != want {
		return E.New("imagemounter: ", command, ": Status ", strconv.Quote(status), ", want ", strconv.Quote(want))
	}
	return nil
}

func (m *mounterConn) developerModeStatus() (bool, error) {
	reply, err := m.request(map[string]any{"Command": "QueryDeveloperModeStatus"})
	if err != nil {
		return false, err
	}
	enabled, ok := reply["DeveloperModeStatus"].(bool)
	if !ok {
		return false, E.New("imagemounter: QueryDeveloperModeStatus reply without DeveloperModeStatus")
	}
	return enabled, nil
}

func (m *mounterConn) imageMounted(imageType string) (bool, error) {
	reply, err := m.request(map[string]any{"Command": "LookupImage", "ImageType": imageType})
	if err != nil {
		return false, err
	}
	signatures, ok := reply["ImageSignature"].([]any)
	if !ok {
		return false, E.New("imagemounter: LookupImage reply without ImageSignature")
	}
	return len(signatures) > 0, nil
}

// CopyDevices lists the DDI 17A324 mounted by MountImage and the DDI 27B52
// installed by cryptexd at /System/Developer, each with its version.plist as
// PersonalizedImageVersionInfo; LookupImage returns no ImageSignature for the
// cryptex (iPhone18,2, iOS 27.3 24C5037i).
func (m *mounterConn) developerImage() (map[string]any, error) {
	reply, err := m.request(map[string]any{"Command": "CopyDevices"})
	if err != nil {
		return nil, err
	}
	entries, _ := reply["EntryList"].([]any)
	image, _ := common.Find(entries, func(item any) bool {
		entry, _ := item.(map[string]any)
		return entry["MountPath"] == developerMountPath
	}).(map[string]any)
	return image, nil
}

func (m *mounterConn) personalizationIdentifiers() (map[string]any, error) {
	reply, err := m.request(map[string]any{
		"Command":               "QueryPersonalizationIdentifiers",
		"PersonalizedImageType": personalizedDDIType,
	})
	if err != nil {
		return nil, err
	}
	identifiers, ok := reply["PersonalizationIdentifiers"].(map[string]any)
	if !ok {
		return nil, E.New("imagemounter: QueryPersonalizationIdentifiers reply without PersonalizationIdentifiers")
	}
	return identifiers, nil
}

// pymobiledevice3 opens a new mounter connection after MissingManifestError
// before requesting a TSS ticket and uploading (pymobiledevice3 9c33842,
// pymobiledevice3/services/mobile_image_mounter.py:404-408).
func (m *mounterConn) personalizationManifest(digest []byte) ([]byte, error) {
	reply, err := m.request(map[string]any{
		"Command":               "QueryPersonalizationManifest",
		"PersonalizedImageType": personalizedDDIType,
		"ImageType":             personalizedDDIType,
		"ImageSignature":        digest,
	})
	if _, isDeviceError := errors.AsType[*DeviceError](err); isDeviceError {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// pymobiledevice3 uses the returned ImageSignature as the upload and mount
	// ticket (pymobiledevice3 9c33842,
	// pymobiledevice3/services/mobile_image_mounter.py:303,408,414).
	ticket, _ := reply["ImageSignature"].([]byte)
	return ticket, nil
}

func (m *mounterConn) nonce() ([]byte, error) {
	reply, err := m.request(map[string]any{
		"Command": "QueryNonce",
		// go-ios sends HostProcessName CoreDeviceService with QueryNonce
		// (go-ios ca2c56d, ios/imagemounter/personalized_image_mounter.go:207-211).
		"HostProcessName":       "CoreDeviceService",
		"PersonalizedImageType": personalizedDDIType,
	})
	if err != nil {
		return nil, err
	}
	nonce, ok := reply["PersonalizationNonce"].([]byte)
	if !ok {
		return nil, E.New("imagemounter: QueryNonce reply without PersonalizationNonce")
	}
	return nonce, nil
}

func (m *mounterConn) upload(ctx context.Context, image resolvedFile, ticket []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(image.path)
	if err != nil {
		return E.Cause(err, "imagemounter")
	}
	defer file.Close()
	stop := context.AfterFunc(ctx, func() { file.Close() })
	defer stop()
	info, err := file.Stat()
	if err != nil {
		return E.Cause(err, "imagemounter")
	}
	if m.bridge != nil {
		return m.uploadBridge(ctx, file, info.Size(), ticket)
	}
	const command = "ReceiveBytes"
	reply, err := m.request(map[string]any{
		"Command":        command,
		"ImageSignature": ticket,
		"ImageSize":      uint64(info.Size()),
		"ImageType":      personalizedImageType,
	})
	if err != nil {
		return err
	}
	if err = m.expectStatus(command, reply, "ReceiveBytesAck"); err != nil {
		return err
	}
	written, err := io.Copy(m.conn, &contextReader{ctx: ctx, reader: file})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return E.Cause(err, "imagemounter: upload the image")
	}
	if written != info.Size() {
		return E.New("imagemounter: uploaded ", written, " of ", info.Size(), " bytes")
	}
	reply, err = m.receive(command)
	if err != nil {
		return err
	}
	return m.expectStatus(command, reply, "Complete")
}

func (m *mounterConn) mount(ticket, trustCache []byte) error {
	const command = "MountImage"
	reply, err := m.request(map[string]any{
		"Command":         command,
		"ImageSignature":  ticket,
		"ImageType":       personalizedImageType,
		"ImageTrustCache": trustCache,
	})
	if err != nil {
		return err
	}
	return m.expectStatus(command, reply, "Complete")
}

// go-ios sends Hangup without reading a reply (go-ios ca2c56d,
// ios/imagemounter/imagemounter.go:266-272).
func (m *mounterConn) hangUp() error {
	if m.bridge != nil {
		// The bridge resets RSD after the successful MountImage reply. Its
		// Hangup handler only replies Goodbye=true (mobile_storage_proxy,
		// iOS 17.7 21H16, 0x100007640..0x100007690,
		// 0x100009f78..0x100009f84, 0x10000a418..0x10000a4f0).
		return nil
	}
	if err := m.conn.Send(map[string]any{"Command": "Hangup"}); err != nil {
		return E.Cause(err, "imagemounter: send Hangup")
	}
	return nil
}
