package sandbox

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// buildHeartbeat is how long a build goes without a progress line before it
// gets one saying what is still running.
var buildHeartbeat = 30 * time.Second

var (
	// buildkit's plain output: `#7 [2/3] RUN ...`, `#7 [stage 2/3] RUN ...`.
	buildkitStep = regexp.MustCompile(`^#\d+ \[(?:\S+ )?(\d+/\d+)\] (.+)$`)
	// the classic docker builder and podman: `Step 2/3 : RUN ...`, `STEP 2/3: RUN ...`.
	classicStep = regexp.MustCompile(`^(?i:step) (\d+/\d+) ?: (.+)$`)
	digest      = regexp.MustCompile(`@sha256:[0-9a-f]+`)
)

// buildSteps is a build's log writer that reports each step to progress as
// the build reaches it, and a step still running every buildHeartbeat.
type buildSteps struct {
	progress func(string)
	now      func() time.Time

	mu    sync.Mutex
	line  []byte
	step  string    // the step running, as reported
	since time.Time // when it started
	said  time.Time // the last progress line
}

func newBuildSteps(progress func(string)) *buildSteps {
	return &buildSteps{progress: progress, now: time.Now}
}

func (s *buildSteps) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.line = append(s.line, p...)
	for {
		i := bytes.IndexByte(s.line, '\n')
		if i < 0 {
			break
		}
		s.parse(strings.TrimRight(string(s.line[:i]), "\r"))
		s.line = s.line[i+1:]
	}
	return len(p), nil
}

func (s *buildSteps) parse(line string) {
	m := buildkitStep.FindStringSubmatch(line)
	if m == nil {
		m = classicStep.FindStringSubmatch(line)
	}
	if m == nil {
		return
	}
	step := "step " + m[1] + ": " + shortInstruction(m[2])
	if step == s.step {
		return // buildkit names a step again when it finishes or is cached
	}
	s.step = step
	s.since, s.said = s.now(), s.now()
	s.progress(step)
}

// start runs the heartbeat until done.
func (s *buildSteps) start() (done func()) {
	s.mu.Lock()
	s.since, s.said = s.now(), s.now()
	s.mu.Unlock()
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(buildHeartbeat / 6)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.beat()
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(stop) }) }
}

func (s *buildSteps) beat() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.said.IsZero() || s.now().Sub(s.said) < buildHeartbeat {
		return
	}
	what := "building"
	if s.step != "" {
		what = "on " + strings.SplitN(s.step, ":", 2)[0]
	}
	s.said = s.now()
	s.progress(fmt.Sprintf("still %s (%s)", what, s.now().Sub(s.since).Round(time.Second)))
}

// shortInstruction keeps a step's instruction to one readable line.
func shortInstruction(s string) string {
	s = digest.ReplaceAllString(strings.Join(strings.Fields(s), " "), "")
	if strings.HasPrefix(s, "RUN ") && strings.Contains(s, ProvisionMount+"/bin:") {
		return "RUN provision" // provisionArgv's wrapper says nothing to a reader
	}
	const max = 72
	if len(s) > max {
		s = s[:max-3] + "..."
	}
	return s
}
