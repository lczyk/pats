package sandbox

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
)

// each step is reported once, as the build reaches it, from buildkit's plain
// output, the classic builder's and podman's alike; everything else is quiet.
func TestBuildSteps(t *testing.T) {
	const provision = `RUN ["/bin/sh","-c","PATH=\"/pats-provision/bin:$PATH\"; export PATH; exec \"$@\"","provision","provision-box"]`
	cases := []struct{ name, out, want string }{
		{"buildkit", `#1 [internal] load build definition from Dockerfile
#1 DONE 0.0s
#5 [1/4] FROM ghcr.io/lczyk/pats/sandbox:v1@sha256:abc
#5 resolve ghcr.io/lczyk/pats/sandbox:v1@sha256:abc done
#6 [2/4] COPY . /pats-provision
#6 DONE 0.1s
#7 [3/4] ` + provision + `
#7 0.312 Get:1 http://archive.ubuntu.com/ubuntu resolute InRelease
#7 [3/4] ` + provision + `
#8 [builder 4/4] RUN rm -rf /pats-provision
`, "step 1/4: FROM ghcr.io/lczyk/pats/sandbox:v1|step 2/4: COPY . /pats-provision|" +
			"step 3/4: RUN provision|step 4/4: RUN rm -rf /pats-provision"},
		{"podman", `STEP 1/4: FROM ghcr.io/lczyk/pats/sandbox:v1
STEP 2/4: COPY . /pats-provision
--> 1a2b3c
STEP 3/4: ` + provision + `
Get:1 http://archive.ubuntu.com/ubuntu resolute InRelease
STEP 4/4: RUN rm -rf /pats-provision
`, "step 1/4: FROM ghcr.io/lczyk/pats/sandbox:v1|step 2/4: COPY . /pats-provision|" +
			"step 3/4: RUN provision|step 4/4: RUN rm -rf /pats-provision"},
		{"classic", "Step 1/2 : FROM ubuntu:26.04\r\n ---> 1a2b3c\r\nStep 2/2 : RUN make\r\n",
			"step 1/2: FROM ubuntu:26.04|step 2/2: RUN make"},
	}
	for _, c := range cases {
		var said []string
		s := newBuildSteps(func(m string) { said = append(said, m) })
		// written in odd pieces, as a pipe hands it over.
		out := c.out
		for len(out) > 0 {
			n := min(7, len(out))
			_, _ = io.WriteString(s, out[:n])
			out = out[n:]
		}
		assert.Equal(t, strings.Join(said, "|"), c.want, c.name)
	}
	long := "RUN  make \t all " + strings.Repeat("x", 80)
	assert.Equal(t, shortInstruction(long), "RUN make all "+strings.Repeat("x", 56)+"...")
}

// a build quiet for the heartbeat gets a line saying what still runs and
// for how long; one that just spoke does not.
func TestBuildStepsHeartbeat(t *testing.T) {
	var said []string
	clock := time.Unix(0, 0)
	s := newBuildSteps(func(m string) { said = append(said, m) })
	s.now = func() time.Time { return clock }
	s.beat() // before start: nothing to say
	done := s.start()
	defer done()
	clock = clock.Add(buildHeartbeat)
	s.beat()
	clock = clock.Add(time.Second)
	_, _ = io.WriteString(s, "STEP 2/3: RUN make\n")
	clock = clock.Add(buildHeartbeat - time.Second)
	s.beat()
	clock = clock.Add(time.Second)
	s.beat()
	clock = clock.Add(buildHeartbeat / 2)
	s.beat()
	clock = clock.Add(buildHeartbeat / 2)
	s.beat()
	assert.Equal(t, strings.Join(said, "|"), "still building (30s)|step 2/3: RUN make|"+
		"still on step 2/3 (30s)|still on step 2/3 (1m0s)")
	done()
}
