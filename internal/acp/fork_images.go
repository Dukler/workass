package acp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"workass/internal/imagesize"
)

// Agent-supplied image attachments are local files carried inside ONE user
// message, so a vision task costs one model turn instead of one Read tool call
// per image. They are validated here before anything is queued: a path that is
// missing, too large, not a raster, or wider than the edge limit is rejected
// with an error naming the file. Workass never resizes: an oversized image
// would otherwise be silently downscaled by the provider, and the caller must
// know exactly what the model saw.
const (
	MaxImageAttachments     = 20
	DefaultMaxImageEdge     = 1000
	MaxImageEdgeCeiling     = 1568
	maxImageAttachmentBytes = 5 << 20
	maxImageAttachmentTotal = 20 << 20
)

// ImageAttachment is one validated local image. Data is never persisted: the
// runner re-reads the file at delivery and refuses it if its digest changed.
type ImageAttachment struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	MIME   string `json:"mimeType"`
	Bytes  int64  `json:"bytes"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	SHA256 string `json:"sha256"`
}

// ValidateImageAttachmentPaths resolves relative paths against baseDir and
// returns the attachments in the given order. maxEdge <= 0 selects the default;
// values above the provider's no-resize ceiling are rejected.
func ValidateImageAttachmentPaths(paths []string, baseDir string, maxEdge int) ([]ImageAttachment, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if maxEdge <= 0 {
		maxEdge = DefaultMaxImageEdge
	}
	if maxEdge > MaxImageEdgeCeiling {
		return nil, fmt.Errorf("max_image_edge_px %d is above %d px, where the provider starts downscaling", maxEdge, MaxImageEdgeCeiling)
	}
	if len(paths) > MaxImageAttachments {
		return nil, fmt.Errorf("too many image attachments: got %d, maximum is %d per message", len(paths), MaxImageAttachments)
	}
	out := make([]ImageAttachment, 0, len(paths))
	var total int64
	seen := make(map[string]bool, len(paths))
	for index, raw := range paths {
		attachment, data, err := readImageAttachment(raw, baseDir)
		if err != nil {
			return nil, fmt.Errorf("attachment %d (%s): %w", index+1, displayAttachmentPath(raw), err)
		}
		if seen[attachment.Path] {
			return nil, fmt.Errorf("attachment %d (%s): the same file is attached twice", index+1, displayAttachmentPath(raw))
		}
		seen[attachment.Path] = true
		if longest := max(attachment.Width, attachment.Height); longest > maxEdge {
			return nil, fmt.Errorf("attachment %d (%s): %dx%d px, longest side %d px exceeds the %d px limit; resize it before sending (Workass never downscales)",
				index+1, displayAttachmentPath(raw), attachment.Width, attachment.Height, longest, maxEdge)
		}
		total += int64(len(data))
		if total > maxImageAttachmentTotal {
			return nil, fmt.Errorf("image attachments total more than %d MiB; send fewer or smaller images", maxImageAttachmentTotal>>20)
		}
		out = append(out, attachment)
	}
	return out, nil
}

// LoadImageAttachment re-reads a validated attachment for delivery and fails if
// the file changed after validation.
func LoadImageAttachment(attachment ImageAttachment) (string, error) {
	data, err := os.ReadFile(attachment.Path)
	if err != nil {
		return "", fmt.Errorf("attachment %s is no longer readable: %w", displayAttachmentPath(attachment.Path), err)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != attachment.SHA256 {
		return "", fmt.Errorf("attachment %s changed after it was validated", displayAttachmentPath(attachment.Path))
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// ImageAttachmentPayloads returns the session-image payload shape the chat
// actor already persists for renderer-pasted images.
func ImageAttachmentPayloads(attachments []ImageAttachment) ([]any, error) {
	images := make([]any, 0, len(attachments))
	for _, attachment := range attachments {
		data, err := LoadImageAttachment(attachment)
		if err != nil {
			return nil, err
		}
		images = append(images, map[string]any{"mimeType": attachment.MIME, "data": data, "name": attachment.Name, "source": "path"})
	}
	return images, nil
}

func readImageAttachment(raw, baseDir string) (ImageAttachment, []byte, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return ImageAttachment{}, nil, errors.New("path is empty")
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		if strings.TrimSpace(baseDir) == "" {
			return ImageAttachment{}, nil, errors.New("relative path needs the chat's working directory; pass an absolute path")
		}
		path = filepath.Join(baseDir, path)
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ImageAttachment{}, nil, errors.New("file does not exist")
		}
		return ImageAttachment{}, nil, err
	}
	if !info.Mode().IsRegular() {
		return ImageAttachment{}, nil, errors.New("not a regular file")
	}
	if info.Size() == 0 {
		return ImageAttachment{}, nil, errors.New("file is empty")
	}
	if info.Size() > maxImageAttachmentBytes {
		return ImageAttachment{}, nil, fmt.Errorf("%.1f MiB is above the %d MiB per-image limit", float64(info.Size())/(1<<20), maxImageAttachmentBytes>>20)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ImageAttachment{}, nil, err
	}
	mimeType := strings.ToLower(strings.TrimSpace(http.DetectContentType(data)))
	if !safeToolImageMIME(mimeType) {
		return ImageAttachment{}, nil, errors.New("not a PNG, JPEG, WebP or GIF raster")
	}
	width, height, err := imagesize.Dimensions(data, mimeType)
	if err != nil {
		return ImageAttachment{}, nil, fmt.Errorf("cannot read image dimensions: %w", err)
	}
	digest := sha256.Sum256(data)
	return ImageAttachment{
		Path: path, Name: filepath.Base(path), MIME: mimeType, Bytes: int64(len(data)),
		Width: width, Height: height, SHA256: hex.EncodeToString(digest[:]),
	}, data, nil
}

func displayAttachmentPath(path string) string {
	path = strings.TrimSpace(path)
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}
