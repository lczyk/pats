package sandbox

import (
	"context"
	"strings"
	"time"
)

// LXDImageUse is what decides whether an lxd image may go: when an instance
// was last made from it (zero: never), when it was uploaded, and whether a
// run holds it under a run-private alias (pats-img-<key>-<token>) between
// publish and claim.
type LXDImageUse struct {
	LastUsed, Uploaded time.Time
	RunPrivate         bool
}

// LXDImageUsage reads image fp's use in the pats project.
func LXDImageUsage(ctx context.Context, o Options, fp string) (LXDImageUse, error) {
	l := newLXD(defaultProcess, lxcCLI, o)
	if err := l.prepareServer(ctx); err != nil {
		return LXDImageUse{}, err
	}
	return l.imageUsage(ctx, fp)
}

func (l *lxd) imageUsage(ctx context.Context, fp string) (LXDImageUse, error) {
	var im struct {
		LastUsedAt time.Time `json:"last_used_at"`
		UploadedAt time.Time `json:"uploaded_at"`
		Aliases    []struct {
			Name string `json:"name"`
		} `json:"aliases"`
	}
	if err := l.query(ctx, "/1.0/images/"+fp+"?project="+lxdProject, &im); err != nil {
		return LXDImageUse{}, err
	}
	u := LXDImageUse{LastUsed: im.LastUsedAt, Uploaded: im.UploadedAt}
	// lxd reports a never-used image's last use as the zero time of its own
	// clock, 0001-01-01.
	if u.LastUsed.Year() <= 1 {
		u.LastUsed = time.Time{}
	}
	for _, a := range im.Aliases {
		if runPrivateAlias(a.Name) {
			u.RunPrivate = true
		}
	}
	return u, nil
}

// runPrivateAlias says whether an alias is a run's private one: a content
// key alias (pats-img-<key>) with a run token after it.
func runPrivateAlias(name string) bool {
	rest, ok := strings.CutPrefix(name, "pats-img-")
	key, token, ok2 := strings.Cut(rest, "-")
	return ok && ok2 && key != "" && token != "" && isHex(key) && isHex(token)
}

func isHex(s string) bool {
	return strings.Trim(s, "0123456789abcdef") == ""
}

// LXDDeleteImage removes image fp from the pats project; one already gone
// is no error.
func LXDDeleteImage(ctx context.Context, o Options, fp string) error {
	l := newLXD(defaultProcess, lxcCLI, o)
	if err := l.prepareServer(ctx); err != nil {
		return err
	}
	if _, err := l.lxc(ctx, "image", "delete", l.ref(fp), "--project", lxdProject); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "image not found") {
			return nil
		}
		return err
	}
	return nil
}
