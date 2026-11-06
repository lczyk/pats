package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// tree writes files (path -> content) under a fresh temp dir and returns it.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
	return root
}

func TestParseDirVector(t *testing.T) {
	root := tree(t, map[string]string{
		"cases/knockout/curl/task.yaml":      "prompt: curl\nprepare: prepare-knockout\nenv:\n  BRANCH: ubuntu-26.04\n",
		"cases/knockout/zlib/task.yaml":      "prompt-file: prompt.md\n",
		"cases/denovo/i2c-tools/task.yaml":   "prompt: i2c\n",
		"cases/denovo/i2c-tools/a.silver":    "x",
		"cases/plain/notes.txt":              "not a task",
		"cases/.hidden/ghost/task.yaml":      "prompt: never read\n",
		"cases/help/mason-help/task.yaml":    "",
		"cases/prepare-shared.sh":            "#!/bin/sh\n",
		"elsewhere/unrelated/task.yaml":      "prompt: outside dir\n",
		"cases/knockout/zlib/prompt.md":      "prompt text",
		"cases/denovo/i2c-tools/extra/x.txt": "plain subdir of a task",
	})
	c, err := ParseIn(root, []byte("cases:\n  kind: task\n  dir: cases\n"))
	require.NoError(t, err)

	var ids, dirs []string
	for _, tk := range c.Tasks {
		ids = append(ids, tk.ID)
		dirs = append(dirs, tk.Dir)
		assert.Equal(t, tk.Vector, "cases")
	}
	// lexical path order; the hidden dir and the plain dirs contribute nothing.
	assert.EqualArrays(t, ids, []string{"i2c-tools", "mason-help", "curl", "zlib"})
	assert.EqualArrays(t, dirs, []string{
		filepath.Join("cases", "denovo", "i2c-tools"),
		filepath.Join("cases", "help", "mason-help"),
		filepath.Join("cases", "knockout", "curl"),
		filepath.Join("cases", "knockout", "zlib"),
	})

	curl := c.Tasks[2]
	assert.Equal(t, curl.Prompt, "curl")
	assert.Equal(t, curl.Prepare, "prepare-knockout")
	assert.Equal(t, curl.Env["BRANCH"], "ubuntu-26.04")
	assert.Equal(t, c.Tasks[3].PromptFile, "prompt.md")
}

// an empty task.yaml is a task with nothing set -- Parse accepts it, Validate
// then asks for a prompt.
func TestParseDirEmptyTaskFile(t *testing.T) {
	root := tree(t, map[string]string{"cases/a/task.yaml": ""})
	c, err := ParseIn(root, []byte("v:\n  kind: task\n  dir: cases\n"))
	require.NoError(t, err)
	require.Len(t, c.Tasks, 1)
	assert.Error(t, c.Validate(), "needs one of prompt, prompt-file or prompt-cmd")
}

func TestParseDirErrors(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		yaml  string
		want  string
	}{
		{
			name:  "nested task.yaml",
			files: map[string]string{"cases/a/task.yaml": "prompt: a\n", "cases/a/inner/task.yaml": "prompt: b\n"},
			yaml:  "v:\n  kind: task\n  dir: cases\n",
			want:  "nested in task cases/a/task.yaml",
		},
		{
			name:  "unknown field names the file and line",
			files: map[string]string{"cases/a/task.yaml": "prompt: a\nbogus: 1\n"},
			yaml:  "v:\n  kind: task\n  dir: cases\n",
			want:  "cases/a/task.yaml: line 2: task \"a\": unknown field \"bogus\"",
		},
		{
			name:  "two keys of one slot",
			files: map[string]string{"cases/a/task.yaml": "prompt: a\nprompt-cmd: b\n"},
			yaml:  "v:\n  kind: task\n  dir: cases\n",
			want:  "prompt, prompt-cmd are mutually exclusive",
		},
		{
			name:  "dir and items",
			files: map[string]string{"cases/a/task.yaml": "prompt: a\n"},
			yaml:  "v:\n  kind: task\n  dir: cases\n  items:\n    b:\n",
			want:  "items and dir are mutually exclusive",
		},
		{
			name:  "dir and defaults",
			files: map[string]string{"cases/a/task.yaml": "prompt: a\n"},
			yaml:  "v:\n  kind: task\n  dir: cases\n  defaults:\n    timeout: 5m\n",
			want:  "defaults and dir are mutually exclusive",
		},
		{
			name: "missing dir",
			yaml: "v:\n  kind: task\n  dir: nope\n",
			want: "dir \"nope\" is not a directory",
		},
		{
			name: "dir outside the root",
			yaml: "v:\n  kind: task\n  dir: ../up\n",
			want: "must be a path inside the pats root",
		},
		{
			name:  "dir with no tasks",
			files: map[string]string{"cases/readme.txt": "x"},
			yaml:  "v:\n  kind: task\n  dir: cases\n",
			want:  "holds no task.yaml",
		},
		{
			name:  "broken yaml in a task file",
			files: map[string]string{"cases/a/task.yaml": "prompt: [\n"},
			yaml:  "v:\n  kind: task\n  dir: cases\n",
			want:  "cases/a/task.yaml:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t, tc.files)
			_, err := ParseIn(root, []byte(tc.yaml))
			assert.Error(t, err, tc.want)
		})
	}
}

// task ids come from directory names alone, so two dirs with one name collide
// like any other duplicate id.
func TestParseDirDuplicateID(t *testing.T) {
	root := tree(t, map[string]string{
		"cases/a/curl/task.yaml": "prompt: a\n",
		"cases/b/curl/task.yaml": "prompt: b\n",
	})
	c, err := ParseIn(root, []byte("v:\n  kind: task\n  dir: cases\n"))
	require.NoError(t, err)
	assert.Error(t, c.Validate(), "duplicate task id: curl")
}

// Load resolves dir against the config file's directory, not the cwd.
func TestLoadResolvesDirFromConfigFile(t *testing.T) {
	root := tree(t, map[string]string{
		"pats.yaml":         "v:\n  kind: task\n  dir: cases\n",
		"cases/a/task.yaml": "prompt: a\n",
	})
	c, err := Load(filepath.Join(root, "pats.yaml"))
	require.NoError(t, err)
	require.Len(t, c.Tasks, 1)
	assert.Equal(t, c.Tasks[0].Dir, filepath.Join("cases", "a"))
}

// a task directory's task.yaml carries its own egress additions.
func TestParseDirTaskEgress(t *testing.T) {
	root := tree(t, map[string]string{
		"cases/knockout/curl/task.yaml": "prompt: p\negress:\n  deny:\n    - 'github.com/*/chisel-releases*'\n",
	})
	c, err := ParseIn(root, []byte("cases:\n  kind: task\n  dir: cases\n"))
	require.NoError(t, err)
	require.Equal(t, len(c.Tasks), 1)
	assert.Equal(t, c.Tasks[0].Egress.Set, true)
	assert.EqualArrays(t, c.Tasks[0].Egress.Deny, []string{"github.com/*/chisel-releases*"})
}
