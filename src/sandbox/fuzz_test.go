package sandbox

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// the tars pats unpacks on the host are not its own: a registry's layer, or
// whatever the tar inside an lxd instance said. the archives fuzzed here are
// built from names and link targets chosen to climb out.

var fuzzTarNames = []string{
	"a", "b", "a/b", "a/b/c", "a/victim", "victim", "./a", "a/", "d", "d/victim",
	"../victim", "../outside/victim", "/abs", "a/../b", "a/../../victim",
	"a/.wh.b", ".wh.a", ".wh..wh..opq", "a/.wh..wh..opq", ".wh..", "a/.wh..",
}

var fuzzTarKinds = []byte{tar.TypeReg, tar.TypeDir, tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeFifo}

// fuzzTar reads data three bytes an entry: its kind, its name and its link
// target, each an index into its vocabulary.
func fuzzTar(data []byte, outside string) []byte {
	targets := []string{
		"b", "a", "a/b", ".", "..", "../outside", "../outside/victim", "../../outside",
		"/", outside, filepath.Join(outside, "victim"), filepath.Join(outside, "d"),
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for ; len(data) >= 3; data = data[3:] {
		h := &tar.Header{
			Typeflag: fuzzTarKinds[int(data[0])%len(fuzzTarKinds)],
			Name:     fuzzTarNames[int(data[1])%len(fuzzTarNames)],
			Mode:     0o644,
		}
		switch h.Typeflag {
		case tar.TypeReg:
			h.Size = 1
		case tar.TypeDir:
			h.Mode = 0o755
		case tar.TypeSymlink, tar.TypeLink:
			h.Linkname = targets[int(data[2])%len(targets)]
		}
		if tw.WriteHeader(h) != nil {
			break
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte("x"))
		}
	}
	_ = tw.Close()
	return b.Bytes()
}

// snapshot is every entry under root: its mode, and a file's bytes or a
// link's target.
func snapshot(t *testing.T, root string) string {
	var lines []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%s %s", strings.TrimPrefix(p, root), info.Mode())
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			line += " -> " + target
		case info.Mode().IsRegular():
			body, _ := os.ReadFile(p)
			line += " " + string(body)
		}
		lines = append(lines, line)
		return nil
	})
	require.NoError(t, err)
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// fuzzUnpack runs unpack on a fuzzed archive into a fresh destination beside
// a directory it has no business in, and fails when that directory, or
// anything else beside the destination, is not as it was.
func fuzzUnpack(t *testing.T, data []byte, unpack func(archive []byte, dest string) error) {
	base := t.TempDir()
	dest, outside := filepath.Join(base, "dest"), filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(dest, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "victim"), []byte("precious"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "d", "victim"), []byte("precious"), 0o644))
	before := snapshot(t, outside)

	_ = unpack(fuzzTar(data, outside), dest) // refusing the archive is fine

	assert.Equal(t, snapshot(t, outside), before, "the archive reached outside its destination")
	beside, err := os.ReadDir(base)
	require.NoError(t, err)
	assert.Equal(t, len(beside), 2, "the archive left an entry beside its destination")
}

func fuzzTarSeeds(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1, 0, 0, 0, 2, 0, 0, 5, 0})   // a dir, a file in it, a file beside it
	f.Add([]byte{2, 1, 1, 0, 0, 0})            // a link to a sibling, a file
	f.Add([]byte{0, 10, 0, 0, 14, 0, 3, 1, 6}) // names and a hard link that climb
	f.Add([]byte{1, 0, 0, 0, 15, 0, 0, 17, 0}) // whiteouts
	f.Add([]byte{4, 0, 0, 5, 1, 0})            // a device and a fifo
}

// FuzzReadTar: lxd's Pull unpacks the instance's tar on the host.
func FuzzReadTar(f *testing.F) {
	fuzzTarSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzUnpack(t, data, func(archive []byte, dest string) error {
			return readTar(bytes.NewReader(archive), dest)
		})
	})
}

// FuzzFromRefs: a dockerfile's FROM lines key the build cache. a comment
// line says nothing to a build wherever it stands, a continued line
// included, so adding one changes no ref.
func FuzzFromRefs(f *testing.F) {
	f.Add([]byte("FROM ubuntu:26.04\nRUN true\n"), 0)
	f.Add([]byte("ARG V=1\nFROM a:${V} AS b\nFROM b\nFROM $UNSET\n"), 2)
	f.Add([]byte("FROM --platform=linux/amd64 \\\n  ubuntu AS x\nFROM scratch\n"), 0)
	f.Add([]byte("ARG "+baseArg+"\nFROM ${"+baseArg+"}\r\n"), 1)
	f.Fuzz(func(t *testing.T, df []byte, at int) {
		refs, exact := fromRefs(df, "base:1")
		lines := strings.SplitAfter(string(df), "\n")
		if at < 0 {
			at = -(at + 1)
		}
		at %= len(lines)
		commented := strings.Join(lines[:at], "") + "# a comment\n" + strings.Join(lines[at:], "")
		crefs, cexact := fromRefs([]byte(commented), "base:1")
		assert.EqualArrays(t, crefs, refs, "a comment line before line %d changes the refs", at)
		assert.Equal(t, cexact, exact)
	})
}

// FuzzNormalizeRef: a ref is normalised once, to one spelling.
func FuzzNormalizeRef(f *testing.F) {
	for _, s := range []string{"ubuntu", "ubuntu:26.04", "ghcr.io/lczyk/pats/sandbox:v1", "localhost:5000/x",
		"index.docker.io/library/ubuntu", "a/b@sha256:00", "docker.io/ubuntu", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, ref string) {
		n := NormalizeRef(ref)
		assert.Equal(t, NormalizeRef(n), n, "%q is not normalised in one go", ref)
	})
}

// FuzzEnvScript: an env value reaches the sandbox through a shell, on its
// stdin. whatever it holds, the command reads back that value and nothing
// of it runs.
func FuzzEnvScript(f *testing.F) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		f.Skip("no /bin/sh")
	}
	for _, s := range []string{"plain", "", "it's", `"$(id)"`, "`id`", "a\nb", "a\\", "$HOME", "'; exit 3; '", " x ", "\t", "caf\xc3\xa9", "\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if strings.ContainsRune(value, 0) {
			return // not a thing an env can hold
		}
		argv := envArgv([]string{"/bin/sh", "-c", `printf %s "$PATS_FUZZ"`})
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = []string{}
		cmd.Stdin = envScript(map[string]string{"PATS_FUZZ": value})
		out, err := cmd.Output()
		require.NoError(t, err, "the env script for %q did not apply", value)
		assert.Equal(t, string(out), value)
	})
}

// FuzzBuildSteps: a build's output arrives in whatever pieces the pipe
// hands over, and the steps reported do not depend on where it was cut.
func FuzzBuildSteps(f *testing.F) {
	f.Add([]byte("#5 [2/4] RUN apt-get update\n#5 DONE 1.2s\n#6 [3/4] COPY . /x\n"), 9)
	f.Add([]byte("Step 1/3 : FROM ubuntu\r\n ---> abc\nStep 2/3 : RUN true\n"), 20)
	f.Add([]byte("no newline at all"), 3)
	steps := func(pieces ...[]byte) []string {
		var said []string
		s := newBuildSteps(func(line string) { said = append(said, line) })
		s.now = func() time.Time { return time.Unix(0, 0) }
		for _, p := range pieces {
			_, _ = s.Write(p)
		}
		return said
	}
	f.Fuzz(func(t *testing.T, out []byte, cut int) {
		if cut < 0 {
			cut = -(cut + 1)
		}
		cut %= len(out) + 1
		assert.EqualArrays(t, steps(out[:cut], out[cut:]), steps(out))
	})
}

// FuzzParseStamps: the stamp script's output is the image's own clis
// talking. a stamp read from it is a version and nothing else.
func FuzzParseStamps(f *testing.F) {
	f.Add("claude=1.2.3 (Claude Code)\ncodex=codex-cli 0.4.0-beta.1\n")
	f.Add("opencode=\n")
	f.Add("=1.0.0\nnoequals\n")
	f.Fuzz(func(t *testing.T, out string) {
		stamps, err := parseStamps(out)
		if err != nil {
			return
		}
		for name, v := range stamps {
			assert.That(t, name != "", "a stamp with no name")
			assert.Equal(t, versionRe.FindString(v), v, "the stamp of %s is not a version", name)
		}
	})
}

// FuzzIgnore: a .dockerignore is the user's file, patterns of any shape,
// matched against the relative paths of a context walk. no pattern list
// panics, and with no negation what is under an ignored directory is ignored
// with it.
func FuzzIgnore(f *testing.F) {
	f.Add("node_modules\n!node_modules/keep\n**/*.log\n# c\n\n", "node_modules/keep/x")
	f.Add("[\n\\\n*\n", "a/b")
	f.Add("a/**/b\n/abs\n", "a/x/y/b")
	f.Fuzz(func(t *testing.T, body, rel string) {
		rel = strings.TrimPrefix(pathpkg.Clean("/"+rel), "/")
		if rel == "" {
			return // not a path a walk yields
		}
		ignore := ignoreMatcher([]byte(body))
		if !ignored(ignore, rel) || ignore.Exclusions() {
			return
		}
		assert.That(t, ignored(ignore, rel+"/below"), "%q is ignored, what is under it is not", rel)
	})
}
