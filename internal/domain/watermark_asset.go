package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// MaxWatermarkFilenameLen caps the asset filename. Bounded short enough to
// avoid PATH_MAX issues when concatenated with the assets directory and
// long enough to encode descriptive names like `province_tv_logo_v2.png`.
const MaxWatermarkFilenameLen = 96

// MaxWatermarkAssetBytes caps a single uploaded watermark. Logos are
// typically a few hundred KB at most; the cap defends the assets directory
// against a runaway upload accidentally filling the volume.
const MaxWatermarkAssetBytes int64 = 8 * 1024 * 1024 // 8 MiB

// WatermarkFilename is both the on-disk basename and the public identifier
// for an uploaded watermark asset. The filename IS the identifier — there
// is no separate UUID layer — so operators reference assets by the name
// they uploaded with (`vtv1_logo.png`) instead of an opaque hash.
type WatermarkFilename string

// watermarkFilenamePattern enforces the rule:
//
//	<basename>.<ext>
//
// where basename is one or more of [a-zA-Z0-9_-] and ext is one or more of
// [a-zA-Z0-9]. Exactly one dot, between basename and ext. This rejects:
//   - path traversal (`../foo.png`, slashes)
//   - hidden files (`.gitignore`)
//   - double extensions (`logo.tar.png`)
//   - missing extension (`logo`)
//   - filesystem-special chars (spaces, quotes)
//
// so any validated filename is safe to drop straight into filepath.Join
// without further sanitisation.
var watermarkFilenamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+\.[a-zA-Z0-9]+$`)

// ValidateWatermarkFilename enforces the safe filename rules above. Returns
// nil when the value is acceptable; otherwise an error suitable for surfacing
// to the API caller as 400.
func ValidateWatermarkFilename(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("watermark filename is required")
	}
	if len(name) > MaxWatermarkFilenameLen {
		return fmt.Errorf("watermark filename exceeds max length %d", MaxWatermarkFilenameLen)
	}
	if !watermarkFilenamePattern.MatchString(name) {
		return errors.New("watermark filename must match <name>.<ext> using only a-z, A-Z, 0-9, '-', '_' (exactly one dot)")
	}
	return nil
}

// WatermarkAsset is the metadata view of one image in the assets library.
// There is no separate persisted metadata file — the asset directory is
// the source of truth, and this struct is reconstituted from `os.Stat`
// plus a content-type sniff each time the library is scanned.
//
//	<assets_dir>/<filename>     ← image bytes (only file per asset)
//
// Filename is unique by construction: the upload endpoint refuses to
// overwrite an existing file, so the asset list never carries duplicates.
type WatermarkAsset struct {
	// Filename is the on-disk basename, ALSO the stable identifier used
	// by Stream.Watermark.Filename and every REST URL.
	Filename WatermarkFilename `json:"filename" yaml:"filename"`

	// ContentType is the MIME type sniffed via http.DetectContentType from
	// the first 512 bytes. Used as the Content-Type response header on /raw.
	ContentType string `json:"content_type" yaml:"content_type"`

	// SizeBytes is the on-disk size of the image (from os.Stat).
	SizeBytes int64 `json:"size_bytes" yaml:"size_bytes"`

	// UploadedAt is the file's modification time in UTC. Save sets it to the
	// wall clock at upload; subsequent rebuilds derive it from mtime so the
	// list survives restart without an external metadata store.
	UploadedAt time.Time `json:"uploaded_at" yaml:"uploaded_at"`
}
