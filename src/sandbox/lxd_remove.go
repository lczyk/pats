package sandbox

import (
	"context"
	"strings"
	"time"
)

// imageUsage reads image fp's use in the pats project. l is ready.
func (l *lxd) imageUsage(ctx context.Context, fp string) (ImageUse, error) {
	var im struct {
		LastUsedAt time.Time `json:"last_used_at"`
		UploadedAt time.Time `json:"uploaded_at"`
		Aliases    []struct {
			Name string `json:"name"`
		} `json:"aliases"`
	}
	if err := l.query(ctx, "/1.0/images/"+fp+"?project="+lxdProject, &im); err != nil {
		return ImageUse{}, noImage(err)
	}
	u := ImageUse{LastUsed: im.LastUsedAt, Uploaded: im.UploadedAt}
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
