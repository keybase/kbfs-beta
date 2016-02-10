package libkbfs

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/keybase/client/go/logger"
	"golang.org/x/net/context"
)

// Runs fn (which may block) in a separate goroutine and waits for it
// to finish, unless ctx is cancelled. Returns nil only when fn was
// run to completion and succeeded.  Any closed-over variables updated
// in fn should be considered visible only if nil is returned.
func runUnlessCanceled(ctx context.Context, fn func() error) error {
	c := make(chan error, 1) // buffered, in case the request is canceled
	go func() {
		c <- fn()
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-c:
		return err
	}
}

// MakeRandomRequestID generates a random ID suitable for tagging a
// request in KBFS, and very likely to be universally unique.
func MakeRandomRequestID() (string, error) {
	// Use a random ID to tag each request.  We want this to be really
	// universally unique, as these request IDs might need to be
	// propagated all the way to the server.  Use a base64-encoded
	// random 128-bit number.
	buf := make([]byte, 128/8)
	err := cryptoRandRead(buf)
	if err != nil {
		return "", err
	}
	// TODO: go1.5 has RawURLEncoding which leaves off the padding entirely
	return strings.TrimSuffix(base64.URLEncoding.EncodeToString(buf), "=="), nil
}

// LogTagsFromContextToMap parses log tags from the context into a map of strings.
func LogTagsFromContextToMap(ctx context.Context) (tags map[string]string) {
	if ctx == nil {
		return tags
	}
	logTags, ok := logger.LogTagsFromContext(ctx)
	if !ok || len(logTags) == 0 {
		return tags
	}
	tags = make(map[string]string)
	for key, tag := range logTags {
		if v := ctx.Value(key); v != nil {
			if value, ok := v.(fmt.Stringer); ok {
				tags[tag] = value.String()
			} else if value, ok := v.(string); ok {
				tags[tag] = value
			}
		}
	}
	return tags
}

// BoolForString returns false if trimmed string is "" (empty), "0", "false", or "no"
func BoolForString(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" || s == "false" || s == "no" {
		return false
	}
	return true
}

// CustomBuild can be set at compile time to override Build
var CustomBuild string

// Build returns the custom or default build
func Build() string {
	if CustomBuild != "" {
		return CustomBuild
	}
	return DefaultBuild
}

// VersionString returns semantic version string
func VersionString() string {
	return fmt.Sprintf("%s-%s", Version, Build())
}

const (
	// CtxBackgroundSyncKey is set in the context for any change
	// notifications that are triggered from a background sync.
	// Observers can ignore these if they want, since they will have
	// already gotten the relevant notifications via LocalChanges.
	CtxBackgroundSyncKey = "kbfs-background"
)

func ctxWithRandomID(ctx context.Context, tagKey interface{},
	tagName string, log logger.Logger) context.Context {
	// Tag each request with a unique ID
	logTags := make(logger.CtxLogTags)
	logTags[tagKey] = tagName
	newCtx := logger.NewContextWithLogTags(ctx, logTags)
	id, err := MakeRandomRequestID()
	if err != nil {
		if log != nil {
			log.Warning("Couldn't generate a random request ID: %v", err)
		}
	} else {
		newCtx = context.WithValue(newCtx, tagKey, id)
	}
	return newCtx
}
