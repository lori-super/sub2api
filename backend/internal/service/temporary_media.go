package service

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// These primitives are shared by the Media Bridge storage runtime. The OVH
// branch intentionally does not restore the legacy temporary-upload service.
var (
	ErrTemporaryMediaUnavailable   = errors.New("temporary media storage is unavailable")
	ErrTemporaryMediaMisconfigured = errors.New("temporary media storage is misconfigured")
	ErrTemporaryMediaInvalidInput  = errors.New("invalid temporary media request")
	ErrTemporaryMediaExpired       = errors.New("temporary media upload expired")
)

// TemporaryMediaObjectMetadata is the small metadata surface needed by the
// administrator storage probe.
type TemporaryMediaObjectMetadata struct {
	ContentType string
	SizeBytes   int64
}

func validateTemporaryMediaEndpoint(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Opaque != "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return fmt.Errorf("%w: TEMP_MEDIA_S3_ENDPOINT must be an HTTPS origin", ErrTemporaryMediaMisconfigured)
	}
	return nil
}

func validTemporaryMediaBucket(value string) bool {
	return value != "" && value != "." && value != ".." && len(value) <= 255 &&
		!strings.ContainsAny(value, "/\\?#") && !strings.ContainsAny(value, "\r\n\t ")
}

func validTemporaryMediaPrefix(value string) bool {
	if value == "" || len(value) > 256 || strings.Contains(value, "\\") || path.Clean(value) != value {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
