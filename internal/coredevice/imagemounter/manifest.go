package imagemounter

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"

	"howett.net/plist"
)

const manifestName = "BuildManifest.plist"

// DeveloperDiskImage update_ddi.py publishes these fixed names
// (DeveloperDiskImage 6eae353, update_ddi.py:60-74). DDI 27A9266e instead
// uses 022-20899-111 and 022-20886-111 asset paths in its BuildManifest.plist
// (BuildManifest.plist, DDI 27A9266e).
var fixedNames = map[string]string{
	"PersonalizedDMG":            "Image.dmg",
	"LoadableTrustCache":         "Image.dmg.trustcache",
	"Cryptex1,GenericDmg":        "Image.dmg",
	"Cryptex1,GenericTrustCache": "Image.dmg.trustcache",
	"Cryptex1,CryptexInfoPlist":  "Image.dmg.cryptex_info",
	"Cryptex1,GenericVolume":     "Image.dmg.root_hash",
}

// DDI build 27A9266e uses Info.Variant
// "iOS Customer Developer Disk Image Cryptex" (BuildManifest.plist,
// DDI 27A9266e).
const cryptexVariantSuffix = "Developer Disk Image Cryptex"

type manifest struct {
	ProductBuildVersion string          `plist:"ProductBuildVersion"`
	BuildIdentities     []buildIdentity `plist:"BuildIdentities"`
}

type buildIdentity map[string]any

func loadManifest(ctx context.Context, dir string) (*manifest, error) {
	data, err := readLocalFile(ctx, filepath.Join(dir, manifestName))
	if err != nil {
		return nil, E.Cause(err, "imagemounter")
	}
	return parseManifest(data)
}

func parseManifest(data []byte) (*manifest, error) {
	var m manifest
	if _, err := plist.Unmarshal(data, &m); err != nil {
		return nil, E.Cause(err, "imagemounter: decode ", manifestName)
	}
	return &m, nil
}

func Outdated(ctx context.Context, mounted, dir string) (available string, outdated bool, err error) {
	manifest, err := loadManifest(ctx, dir)
	if err != nil {
		return "", false, err
	}
	mountedVersion, err := parseBuild(mounted)
	if err != nil {
		return "", false, err
	}
	availableVersion, err := parseBuild(manifest.ProductBuildVersion)
	if err != nil {
		return "", false, err
	}
	return manifest.ProductBuildVersion, buildOlder(mountedVersion, availableVersion), nil
}

// DDI builds 27A5228h, 27A9266e and 27B52 have cryptex versions 27.1.5228.8,
// 27.1.9266.5 and 27.2.52.0 (Image.dmg.cryptex_info, those DDI builds).
func parseBuild(build string) ([4]uint64, error) {
	var version [4]uint64
	train := strings.IndexFunc(build, func(r rune) bool { return r < '0' || r > '9' })
	if train > 0 && build[train] >= 'A' && build[train] <= 'Z' {
		number := build[train+1:]
		if last := len(number) - 1; last > 0 && number[last] >= 'a' && number[last] <= 'z' {
			version[3] = uint64(number[last]-'a') + 1
			number = number[:last]
		}
		major, majorErr := strconv.ParseUint(build[:train], 10, 64)
		minor, minorErr := strconv.ParseUint(number, 10, 64)
		if majorErr == nil && minorErr == nil {
			version[0], version[1], version[2] = major, uint64(build[train]-'A')+1, minor
			return version, nil
		}
	}
	return version, E.New("imagemounter: unexpected Developer Disk Image build ", strconv.Quote(build))
}

// A build with a trailing letter precedes the plain builds of its train
// whatever its number: DDI 27B5028f has CoreDevice 651.19 and DDI 27B48 has
// 651.31 (CoreDeviceUtilities current version, those DDI builds).
func buildOlder(version, than [4]uint64) bool {
	if order := slices.Compare(version[:2], than[:2]); order != 0 {
		return order < 0
	}
	if (version[3] != 0) != (than[3] != 0) {
		return version[3] != 0
	}
	return slices.Compare(version[2:], than[2:]) < 0
}

func (m *manifest) personalizedIdentity(board, chip uint64) (buildIdentity, error) {
	identity := common.Find(m.BuildIdentities, func(identity buildIdentity) bool {
		identityBoard, boardOK := numberValue(identity["ApBoardID"])
		identityChip, chipOK := numberValue(identity["ApChipID"])
		if !boardOK || !chipOK || identityBoard != board || identityChip != chip {
			return false
		}
		_, hasImage := identity.entry("PersonalizedDMG")
		return hasImage
	})
	if identity == nil {
		return nil, E.New("imagemounter: the Developer Disk Image does not list this device (board 0x", strconv.FormatUint(board, 16), ", chip 0x", strconv.FormatUint(chip, 16), ")")
	}
	return identity, nil
}

// All PersonalizedDMG identities in DDI build 27A9266e name the same
// image and trust-cache paths (BuildManifest.plist, DDI 27A9266e).
func (m *manifest) anyPersonalizedIdentity() (buildIdentity, error) {
	identity := common.Find(m.BuildIdentities, func(identity buildIdentity) bool {
		_, hasImage := identity.entry("PersonalizedDMG")
		return hasImage
	})
	if identity == nil {
		return nil, E.New("imagemounter: the build manifest has no personalized image")
	}
	return identity, nil
}

func (m *manifest) cryptexIdentity() (buildIdentity, error) {
	identity := common.Find(m.BuildIdentities, func(identity buildIdentity) bool {
		info, _ := identity["Info"].(map[string]any)
		variant, _ := info["Variant"].(string)
		return strings.HasSuffix(variant, cryptexVariantSuffix)
	})
	if identity == nil {
		return nil, E.New("imagemounter: the build manifest has no cryptex")
	}
	return identity, nil
}

func (i buildIdentity) manifestEntries() map[string]any {
	entries, _ := i["Manifest"].(map[string]any)
	return entries
}

func (i buildIdentity) entry(key string) (map[string]any, bool) {
	entry, ok := i.manifestEntries()[key].(map[string]any)
	return entry, ok
}

// DDI build 27A9266e has SHA-384 digests for its Cryptex assets and 122 of
// its 140 PersonalizedDMG identities; the 18 identities with SHA-1 digests
// have ApChipID 0x7000, 0x7001, 0x8000, 0x8001 or 0x8003 (A8, A8X, A9, A9X)
// (BuildManifest.plist, DDI 27A9266e).
func (i buildIdentity) digest(key string) ([]byte, error) {
	entry, ok := i.entry(key)
	if !ok {
		return nil, E.New("imagemounter: the build identity has no ", key)
	}
	digest, _ := entry["Digest"].([]byte)
	if len(digest) != sha512.Size384 {
		return nil, E.New("imagemounter: ", key, " has no SHA-384 digest")
	}
	return digest, nil
}

var errMissingFile = E.New("imagemounter: missing file")

type resolvedFile struct {
	key    string
	path   string
	digest []byte
}

func (i buildIdentity) names(key string) []string {
	entry, _ := i.entry(key)
	info, _ := entry["Info"].(map[string]any)
	names := make([]string, 0, 2)
	if path, _ := info["Path"].(string); path != "" && fs.ValidPath(path) {
		names = append(names, path)
	}
	if name, ok := fixedNames[key]; ok && !slices.Contains(names, name) {
		names = append(names, name)
	}
	return names
}

func (i buildIdentity) resolve(dir, key string) (resolvedFile, error) {
	digest, err := i.digest(key)
	if err != nil {
		return resolvedFile{}, err
	}
	for _, name := range i.names(key) {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if _, err := os.Stat(path); err == nil {
			return resolvedFile{key: key, path: path, digest: digest}, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return resolvedFile{}, E.Cause(err, "imagemounter")
		}
	}
	return resolvedFile{}, E.Extend(errMissingFile, key, " in ", dir)
}

func (i buildIdentity) readFile(ctx context.Context, dir, key string) ([]byte, error) {
	file, err := i.resolve(dir, key)
	if err != nil {
		return nil, err
	}
	data, err := readLocalFile(ctx, file.path)
	if err != nil {
		return nil, E.Cause(err, "imagemounter")
	}
	sum := sha512.Sum384(data)
	if !bytes.Equal(sum[:], file.digest) {
		return nil, file.mismatch(sum[:])
	}
	return data, nil
}

func (f resolvedFile) verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(f.path)
	if err != nil {
		return E.Cause(err, "imagemounter")
	}
	defer file.Close()
	stop := context.AfterFunc(ctx, func() { file.Close() })
	defer stop()
	hash := sha512.New384()
	_, err = io.Copy(hash, &contextReader{ctx: ctx, reader: file})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return E.Cause(err, "imagemounter: read ", f.path)
	}
	if sum := hash.Sum(nil); !bytes.Equal(sum, f.digest) {
		return f.mismatch(sum)
	}
	return nil
}

func readLocalFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	stop := context.AfterFunc(ctx, func() { file.Close() })
	defer stop()
	data, err := io.ReadAll(&contextReader{ctx: ctx, reader: file})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return data, err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := r.reader.Read(data)
	if r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	return count, err
}

func (f resolvedFile) mismatch(sum []byte) error {
	return &mismatchError{path: f.path, sum: sum, want: f.digest}
}

type mismatchError struct {
	path      string
	sum, want []byte
}

func (e *mismatchError) Error() string {
	return fmt.Sprintf("imagemounter: %s does not match the build manifest (SHA-384 %s, want %s)",
		e.path, hex.EncodeToString(e.sum[:min(8, len(e.sum))]), hex.EncodeToString(e.want[:min(8, len(e.want))]))
}

func parseNumber(text string) (uint64, bool) {
	base := 10
	if trimmed, ok := strings.CutPrefix(strings.ToLower(text), "0x"); ok {
		text, base = trimmed, 16
	}
	value, err := strconv.ParseUint(text, base, 64)
	return value, err == nil
}
