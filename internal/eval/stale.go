package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/version"
	"github.com/lczyk/pats/src/sandbox"
)

// localInputs digests what of a sandbox's image lives in the project: the
// provision material (its script or command, and bin/) with the base it
// goes on, or a build: context. init records them in the pin and run
// compares, so an edit since init is noticed without building anything.
func localInputs(sb config.Sandbox, configDir string) (map[string]string, error) {
	in := map[string]string{}
	switch {
	case sb.Provision != "" || sb.ProvisionCmd != "":
		material, argv, err := stageMaterial(configDir, configDir, sb.Provision, sb.ProvisionCmd, sandbox.ProvisionMount)
		if err != nil {
			return nil, err
		}
		defer sandbox.RemoveTemp(material)
		h, err := hashTree(material)
		if err != nil {
			return nil, err
		}
		// the command's arguments are part of what it does, not in the files.
		sum := sha256.Sum256(fmt.Appendf(nil, "%s\n%q", h, argv))
		in["provision"] = hex.EncodeToString(sum[:])
		in["base_ref"] = sb.Base
	case sb.Base != "":
		in["base_ref"] = sb.Base
	case sb.Build != "":
		ctxDir, dockerfile, err := resolveBuildSpec(configDir, sb.Build)
		if err != nil {
			return nil, err
		}
		h, err := sandbox.ContextHash(ctxDir, dockerfile)
		if err != nil {
			return nil, err
		}
		in["context"] = h
	case sb.Image != "":
		in["image_ref"] = sb.Image
	}
	return in, nil
}

// hashTree digests a directory: each entry's path, mode and contents.
func hashTree(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s %o\n", filepath.ToSlash(rel), info.Mode())
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%d\n", len(b))
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

// staleLocal is what changed since the pin was made, as one phrase each:
// the local inputs, and the pats that made it.
func staleLocal(e *pinEntry, now map[string]string) []string {
	var out []string
	names := map[string]string{
		"provision": "the provision material", "context": "the build context",
		"base_ref": "the base", "image_ref": "the image",
	}
	for _, k := range []string{"provision", "context", "base_ref", "image_ref"} {
		if e.Inputs[k] != now[k] {
			out = append(out, names[k])
		}
	}
	if e.PatsVersion != version.Info.Version {
		out = append(out, fmt.Sprintf("the pats version (%s, now %s)", e.PatsVersion, version.Info.Version))
	} else if e.PatsBuild != patsBuild() {
		out = append(out, fmt.Sprintf("the pats build (%s, now %s)", e.PatsBuild, patsBuild()))
	}
	return out
}

// proxyMismatch says why a pinned proxy may not serve this pats: the proxy
// is pats's own code on the other end of the PATS_EGRESS_* protocol, so it
// must come from this version and this build. "" when it may.
func proxyMismatch(e *pinEntry) string {
	switch {
	case e.Proxy == nil:
		return ""
	case e.PatsVersion != version.Info.Version:
		return fmt.Sprintf("its egress proxy is from pats %s, this is %s", e.PatsVersion, version.Info.Version)
	case e.PatsBuild != patsBuild():
		return fmt.Sprintf("its egress proxy is from another pats build (%s, this is %s)", e.PatsBuild, patsBuild())
	}
	return ""
}
