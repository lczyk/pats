package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// the image half of fakeDaemon: what the container driver's image methods
// ask of the daemon, answered from images held by id, a registry to pull
// from, and builds that make an image of their inputs.

type fakeDImage struct {
	tags    []string          // repo:tag refs naming it
	digests []string          // repo@sha256:... it was pulled as
	labels  map[string]string // from --label
}

// fakeRegistryImage is what a pull of a ref brings.
type fakeRegistryImage struct{ id, digest string }

func fakeImageID(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// findImage is the id ref names: a tag, a whole id, or a prefix of one;
// "" when none. f.mu is held.
func (f *fakeDaemon) findImage(ref string) string {
	if _, ok := f.images[ref]; ok {
		return ref
	}
	for id, im := range f.images {
		for _, t := range im.tags {
			if t == ref || t == ref+":latest" {
				return id
			}
		}
		if h := strings.TrimPrefix(ref, "sha256:"); len(h) >= 12 && strings.HasPrefix(strings.TrimPrefix(id, "sha256:"), h) {
			return id
		}
	}
	return ""
}

func (f *fakeDaemon) noSuchImage(stderr io.Writer, ref string) error {
	if f.driver == "podman" {
		fmt.Fprintf(stderr, "Error: %s: image not known\n", ref)
		return exitStatus(125)
	}
	return dockerFail(stderr, "No such image: %s", ref)
}

// tag points ref at id, taking it off any other image. f.mu is held.
func (f *fakeDaemon) tag(id, ref string) {
	for _, im := range f.images {
		for i, t := range im.tags {
			if t == ref {
				im.tags = append(im.tags[:i], im.tags[i+1:]...)
				break
			}
		}
	}
	if f.images[id] == nil {
		f.images[id] = &fakeDImage{labels: map[string]string{}}
	}
	f.images[id].tags = append(f.images[id].tags, ref)
}

// imageCmd answers the image and identity commands; handled is false for
// any other. f.mu is held.
func (f *fakeDaemon) imageCmd(stdout, stderr io.Writer, args []string) (handled bool, err error) {
	at := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	switch {
	case at(0) == "context" && at(1) == "show" && len(args) == 2:
		fmt.Fprintln(stdout, "default")
	case at(0) == "info" && at(1) == "--format" && len(args) == 3:
		switch args[2] {
		case "{{.ID}}|{{.ServerVersion}}":
			fmt.Fprintf(stdout, "%s|27.1.1\n", f.daemonID)
		case engines[f.driver].cpus:
			fmt.Fprintln(stdout, f.cpus)
		default:
			f.t.Errorf("fake %s: info format %q", f.driver, args[2])
			return true, dockerFail(stderr, "template: unknown field")
		}
	case at(0) == "system" && at(1) == "connection" && at(2) == "list":
		if f.driver != "podman" {
			f.t.Errorf("fake %s: %s", f.driver, strings.Join(args, " "))
		}
		b, _ := json.Marshal(f.conns)
		stdout.Write(b)
	case at(0) == "image" && at(1) == "inspect" && at(2) == "--format" && len(args) == 5:
		id := f.findImage(args[4])
		if id == "" {
			return true, f.noSuchImage(stderr, args[4])
		}
		switch args[3] {
		case "{{.Id}}":
			fmt.Fprintln(stdout, id)
		case "{{.Id}}|{{range .RepoDigests}}{{.}} {{end}}":
			fmt.Fprintf(stdout, "%s|", id)
			for _, d := range f.images[id].digests {
				fmt.Fprintf(stdout, "%s ", d)
			}
			fmt.Fprintln(stdout)
		default:
			f.t.Errorf("fake %s: image inspect format %q", f.driver, args[3])
		}
	case at(0) == "images":
		var label string
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "-a", "-q", "--no-trunc":
			case "--filter":
				i++
				label, _ = strings.CutPrefix(at(i), "label=")
			default:
				f.t.Errorf("fake %s: images %s", f.driver, args[i])
			}
		}
		var ids []string
		for id, im := range f.images {
			k, v, _ := strings.Cut(label, "=")
			if label == "" || im.labels[k] == v {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Fprintln(stdout, id)
		}
	case at(0) == "pull":
		ref := args[len(args)-1]
		r, ok := f.registry[ref]
		if !ok {
			return true, dockerFail(stderr, "manifest for %s not found: manifest unknown", ref)
		}
		f.tag(r.id, ref)
		repo := Repository(ref)
		d := repo + "@" + r.digest
		im := f.images[r.id]
		if !contains(im.digests, d) {
			im.digests = append(im.digests, d)
		}
		f.pulls++
		fmt.Fprintf(stdout, "%s: Pulling from %s\nDigest: %s\n", ref, repo, r.digest)
	case at(0) == "rmi" && len(args) == 2:
		id := f.findImage(args[1])
		if id == "" {
			return true, f.noSuchImage(stderr, args[1])
		}
		for _, c := range f.ctrs {
			if f.findImage(c.image) == id {
				return true, dockerFail(stderr, "conflict: unable to delete %s (must be forced) - image is being used by stopped container %s", args[1], c.id)
			}
		}
		delete(f.images, id)
	case at(0) == "build":
		return true, f.build(stderr, args[1:])
	default:
		return false, nil
	}
	return true, nil
}

// build makes an image named by its inputs, its id to --iidfile. f.mu is
// held.
func (f *fakeDaemon) build(stderr io.Writer, args []string) error {
	var iid, dockerfile string
	labels := map[string]string{}
	key := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--iidfile":
			i++
			iid = args[i]
		case "-f":
			i++
			dockerfile = args[i]
		case "--no-cache":
		case "--build-arg":
			i++
			key = append(key, args[i])
		case "--label":
			i++
			k, v, _ := strings.Cut(args[i], "=")
			labels[k] = v
		default:
			if strings.HasPrefix(args[i], "-") || i != len(args)-1 {
				f.t.Errorf("fake %s: build %s", f.driver, strings.Join(args, " "))
				return dockerFail(stderr, "unknown flag")
			}
			key = append(key, "ctx")
		}
	}
	df, err := os.ReadFile(dockerfile)
	if err != nil {
		return dockerFail(stderr, "unable to read %s", dockerfile)
	}
	f.builds++
	id := fakeImageID(string(df) + strings.Join(key, "\n") + fmt.Sprint(f.builds))
	f.images[id] = &fakeDImage{labels: labels}
	return os.WriteFile(iid, []byte(id), 0o644)
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
