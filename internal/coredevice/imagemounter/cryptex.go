// The cryptexd routines follow idevice
// idevice/src/services/cryptexd.rs (https://github.com/jkcoxson/idevice, MIT
// License, Copyright 2026 Jackson Coxson).

package imagemounter

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice/nserror"
	"github.com/swipt-automation/swipt/internal/coredevice/xpc"
)

// pymobiledevice3 sends client-version 3, image type 10, persistence 2 and
// nonce-persistence 1 for the DDI cryptex (pymobiledevice3 9c33842,
// pymobiledevice3/services/cryptexd.py:142,150,164-165,435-437).
const (
	cryptexClientVersion  = 3
	cryptexImageTypeIndex = 10
	cryptexPersistence    = 2
	// idevice sends nonce_persistence for both persistence fields
	// (idevice d32c818, idevice/src/services/cryptexd.rs:273-274).
	cryptexNoncePersistence = 1
)

var cryptexFiles = []struct {
	argument string
	key      string
}{
	{"image", "Cryptex1,GenericDmg"},
	{"trustcache", "Cryptex1,GenericTrustCache"},
	{"im4m", ""},
	{"info", "Cryptex1,CryptexInfoPlist"},
	{"volumehash", "Cryptex1,GenericVolume"},
}

type CryptexError struct {
	Routine string
	Code    int64
	levels  []nserror.Level
}

func (e *CryptexError) Error() string {
	if len(e.levels) == 0 {
		return fmt.Sprintf("imagemounter: cryptexd %s: error %d", e.Routine, e.Code)
	}
	return fmt.Sprintf("imagemounter: cryptexd %s: %s", e.Routine, nserror.Describe(e.levels))
}

func (c *Client) MountCryptex(ctx context.Context, dir string) error {
	if err := c.checkCanMount(ctx); err != nil {
		return err
	}
	install, err := c.prepareCryptex(ctx, dir)
	if err != nil {
		return err
	}
	return install(ctx)
}

func (c *Client) ReplaceCryptex(ctx context.Context, dir string, personalized bool) error {
	install, err := c.prepareCryptex(ctx, dir)
	if err != nil {
		return err
	}
	if personalized {
		// cryptexd installed on the same tunnel after UnmountImage
		// (iPhone18,2, iOS 27.3 24C5037i).
		err = c.Unmount(ctx)
	} else {
		// pymobiledevice3 names the cryptex to uninstall by
		// remote-cryptex-identifier (pymobiledevice3 1146717,
		// pymobiledevice3/services/cryptexd.py:675-690). cryptexd answered on the
		// same tunnel after uninstall (iPhone18,2 virtual, iOS 27.0.1 24A446).
		_, err = c.invokeCryptexd(ctx, "uninstall", map[string]any{"remote-cryptex-identifier": ddiCryptexIdentifier}, nil)
	}
	if err != nil {
		return err
	}
	return install(ctx)
}

func (c *Client) prepareCryptex(ctx context.Context, dir string) (func(ctx context.Context) error, error) {
	manifest, err := loadManifest(ctx, dir)
	if err != nil {
		return nil, err
	}
	identity, err := manifest.cryptexIdentity()
	if err != nil {
		return nil, err
	}
	properties, err := cryptexProperties(identity)
	if err != nil {
		return nil, err
	}
	files := make([]resolvedFile, len(cryptexFiles))
	for index, file := range cryptexFiles {
		if file.key == "" {
			continue
		}
		if files[index], err = identity.resolve(dir, file.key); err != nil {
			return nil, err
		}
		if err = files[index].verify(ctx); err != nil {
			return nil, err
		}
	}
	chip, err := c.invokeCryptexd(ctx, "read-personalization-id", map[string]any{}, nil)
	if err != nil {
		return nil, err
	}
	nonceDomain, _ := numberValue(identity["Cryptex1,NonceDomain"])
	nonce, err := c.cryptexNonce(ctx, nonceDomain)
	if err != nil {
		return nil, err
	}
	request, err := cryptexRequest(identity, chip, nonce)
	if err != nil {
		return nil, err
	}
	c.logger.Info("requesting a Developer Disk Image cryptex ticket from Apple")
	response, err := sendTSS(ctx, request)
	if err != nil {
		return nil, err
	}
	// idevice and pymobiledevice3 accept Cryptex1,Ticket or ApImg4Ticket
	// (idevice 3854a5d, idevice/src/services/cryptexd.rs:410-417;
	// pymobiledevice3 9c33842, pymobiledevice3/services/cryptexd.py:574).
	ticket, ok := response["Cryptex1,Ticket"].([]byte)
	if !ok {
		if ticket, ok = response["ApImg4Ticket"].([]byte); !ok {
			return nil, E.New("imagemounter: TSS response without Cryptex1,Ticket or ApImg4Ticket")
		}
	}
	return func(ctx context.Context) error {
		if err := c.install(ctx, files, ticket, properties); err != nil {
			return err
		}
		c.logger.Info("installed the Developer Disk Image cryptex")
		return nil
	}, nil
}

// pymobiledevice3 encodes Cryptex1,SubType and Cryptex1,NonceDomain as
// XPC uint64 in cryptex1-properties (pymobiledevice3 9c33842,
// pymobiledevice3/services/cryptexd.py:347-354).
func cryptexProperties(identity buildIdentity) (map[string]any, error) {
	properties := map[string]any{"MountedCryptex": false}
	for _, key := range []string{"Cryptex1,SubType", "Cryptex1,NonceDomain"} {
		number, ok := numberValue(identity[key])
		if !ok {
			return nil, E.New("imagemounter: the cryptex identity has no ", key)
		}
		properties[key] = number
	}
	for key, manifestKey := range map[string]string{
		"Cryptex1,UseProductClass": "Cryptex1,UseProductClass",
		"Cryptex1,Version":         "Cryptex1,Version",
		"Cryptex1,PreauthVersion":  "Cryptex1,PreauthorizationVersion",
	} {
		value, ok := identity[manifestKey]
		if !ok {
			return nil, E.New("imagemounter: the cryptex identity has no ", manifestKey)
		}
		properties[key] = value
	}
	return properties, nil
}

// pymobiledevice3 announces image, trustcache, im4m, info and volumehash as
// transfer IDs 1 through 5, sends all five, then reads the install reply
// (pymobiledevice3 9c33842, pymobiledevice3/services/cryptexd.py:476-500).
func (c *Client) install(ctx context.Context, files []resolvedFile, ticket []byte, properties map[string]any) error {
	argv := map[string]any{
		"auth":                uint64(0),
		"client-version":      uint64(cryptexClientVersion),
		"cryptex1-properties": properties,
		"image-type-index":    int64(cryptexImageTypeIndex),
		"nonce-persistence":   uint64(cryptexNoncePersistence),
		"persistence":         uint64(cryptexPersistence),
	}
	sizes := make([]int64, len(cryptexFiles))
	for index, file := range cryptexFiles {
		if file.key == "" {
			sizes[index] = int64(len(ticket))
		} else {
			info, err := os.Stat(files[index].path)
			if err != nil {
				return E.Cause(err, "imagemounter")
			}
			sizes[index] = info.Size()
		}
		argv[file.argument] = xpc.FileTransfer{ID: uint64(index + 1), Size: uint64(sizes[index])}
	}
	_, err := c.invokeCryptexd(ctx, "install", argv, func(conn *xpc.Conn) error {
		for index, file := range cryptexFiles {
			if err := ctx.Err(); err != nil {
				return err
			}
			if file.key == "" {
				if err := conn.SendFile(ctx, uint64(index+1), bytes.NewReader(ticket), sizes[index]); err != nil {
					return E.Cause(err, "imagemounter: send ", file.argument)
				}
				continue
			}
			content, err := os.Open(files[index].path)
			if err != nil {
				return E.Cause(err, "imagemounter")
			}
			stop := context.AfterFunc(ctx, func() { content.Close() })
			err = conn.SendFile(ctx, uint64(index+1), &contextReader{ctx: ctx, reader: content}, sizes[index])
			stop()
			content.Close()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return E.Cause(err, "imagemounter: send ", file.argument)
			}
		}
		return nil
	})
	return err
}

// DDI build 27A9266e sets Cryptex1,NonceDomain to 4 (BuildManifest.plist,
// DDI 27A9266e); pymobiledevice3 sends that value as nonce-domain-handle, not
// nonce-domain (pymobiledevice3 9c33842,
// pymobiledevice3/services/cryptexd.py:520,536,571).
func (c *Client) cryptexNonce(ctx context.Context, domain uint64) ([]byte, error) {
	argv, err := c.invokeCryptexd(ctx, "get-nonce", map[string]any{"nonce-domain-handle": domain}, nil)
	if err != nil {
		return nil, err
	}
	blob, ok := argv["nonce"].([]byte)
	if !ok {
		return nil, E.New("imagemounter: get-nonce reply without a nonce")
	}
	return unwrapNonce(blob)
}

// idevice unwrap_nonce reads the nonce at byte 2 and its length from the
// last four bytes as little endian (idevice 3854a5d,
// idevice/src/services/cryptexd.rs:360-370).
func unwrapNonce(blob []byte) ([]byte, error) {
	if len(blob) < 6 {
		return nil, E.New("imagemounter: nonce structure of ", len(blob), " bytes")
	}
	length := binary.LittleEndian.Uint32(blob[len(blob)-4:])
	if uint64(length) > uint64(len(blob)-6) {
		return nil, E.New("imagemounter: nonce of ", length, " bytes in a ", len(blob), "-byte structure")
	}
	return blob[2 : 2+length], nil
}

func (c *Client) ddiCryptex(ctx context.Context) (map[string]any, error) {
	argv, err := c.invokeCryptexd(ctx, "copy-installed", map[string]any{}, nil)
	if err != nil {
		return nil, err
	}
	items, _ := argv["remote-cryptex-array"].([]any)
	entry, _ := common.Find(items, func(item any) bool {
		entry, _ := item.(map[string]any)
		return entry["remote-cryptex-identifier"] == ddiCryptexIdentifier
	}).(map[string]any)
	return entry, nil
}

// pymobiledevice3 opens and closes a RemoteXPC connection for each cryptexd
// routine, including install (pymobiledevice3 9c33842,
// pymobiledevice3/services/cryptexd.py:395-409,494-507).
func (c *Client) invokeCryptexd(ctx context.Context, routine string, argv map[string]any, send func(conn *xpc.Conn) error) (map[string]any, error) {
	conn, err := c.device.DialXPC(ctx, cryptexService)
	if err != nil {
		return nil, E.Cause(err, "imagemounter")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	reply, err := exchangeCryptexd(conn, routine, argv, send)
	if err != nil && ctx.Err() != nil {
		return nil, E.Cause(ctx.Err(), "imagemounter: cryptexd ", routine)
	}
	return reply, err
}

func exchangeCryptexd(conn *xpc.Conn, routine string, argv map[string]any, send func(conn *xpc.Conn) error) (map[string]any, error) {
	err := conn.Send(map[string]any{"routine": routine, "argv": argv}, true)
	if err != nil {
		return nil, E.Cause(err, "imagemounter: send cryptexd ", routine)
	}
	if send != nil {
		if err = send(conn); err != nil {
			return nil, err
		}
	}
	reply, err := conn.Receive()
	if err != nil {
		return nil, E.Cause(err, "imagemounter: receive cryptexd ", routine, " reply")
	}
	return cryptexReply(routine, reply)
}

// idevice treats a missing error field as zero, including for
// read-personalization-id (idevice 3854a5d,
// idevice/src/services/cryptexd.rs:342-346).
func cryptexReply(routine string, reply map[string]any) (map[string]any, error) {
	if cferr, ok := reply["cferr"].(map[string]any); ok {
		levels := nserror.Chain(map[string]any{"domain": cferr["cferr_domain"], "code": cferr["cferr_code"], "userInfo": cferr["cferr_userinfo"]})
		return nil, &CryptexError{Routine: routine, Code: levels[0].Code, levels: levels}
	}
	if code := signedValue(reply["error"]); code != 0 {
		return nil, &CryptexError{Routine: routine, Code: code}
	}
	argv, _ := reply["argv"].(map[string]any)
	return argv, nil
}

func signedValue(value any) int64 {
	switch value := value.(type) {
	case int64:
		return value
	case uint64:
		return int64(value)
	}
	return 0
}
