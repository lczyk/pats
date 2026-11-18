# refusal message

status: implemented, for 1.3.0, but for the spread test at the end of "tests": the answer was checked by hand
instead, with curl, git and python against the built proxy, on a terminated host, a plain one and a refused CONNECT.

## problem

a request the egress proxy refuses under the default `deny-response` is answered `403 Forbidden` with the body
`egress denied`. that says what happened and not what to do about it, and an agent reads a bare 403 as a fault as
readily as a decision. a model refused a url then asks for the same content by other spellings, other hosts and
other tools, each one a turn and none of them useful: the policy refuses those too, and the run pays for finding
that out.

the proxy cannot stop a model trying. it can say, in the one place the model is sure to look, that the refusal is a
decision and that a way around it is not wanted.

## what a client shows

measured once, against a server answering every request and every CONNECT `403 <phrase>` with a text body, the
client pointed at it with `http_proxy` and `https_proxy`:

| invocation | a refused request | a refused CONNECT |
|---|---|---|
| `curl -sS <url>` | the body | `CONNECT tunnel failed, response 403` |
| `curl -sS -f <url>` | `The requested URL returned error: 403` | the same line |
| `git ls-remote <url>` | the body, as `remote:` lines, and the 403 | `CONNECT tunnel failed, response 403` |
| python `urlopen(url)`, the exception printed | `HTTP Error 403: <phrase>` | `Tunnel connection failed: 403 <phrase>` |
| `wget -nv -O- <url>` | `ERROR 403: <phrase>.` | `Proxy tunneling failed: <phrase>` |

curl 8.23, git 2.56, python 3.15, wget 1.25. a refused request is one the proxy saw in clear: on a host it
terminates tls for, or over plain http. a refused CONNECT is a host the policy does not allow at all. these are
those invocations, not every use of each client: a script that catches the exception and prints nothing shows
nothing. python `requests` and node were not measured.

so no one place reaches every client. the reason phrase reaches python and wget, the body reaches curl and git, and a
CONNECT refused to curl or git shows neither. both are worth setting, and neither is a fix: a model that has read the
message can still ignore it.

## proposal

under `deny-response: 403`, the default, a refusal is:

```
HTTP/1.1 403 Blocked by network policy, do not retry
Content-Type: text/plain; charset=utf-8
Content-Length: 250
Connection: close

this request was refused by the network policy of this environment.
it is a decision, not a fault: retrying will not change it.
do not look for another route to the same content.
carry on without it, and say so if the task cannot be done without it.
```

- **the same for every refusal.** it names no entry, no host and no reason. the entry that decided is a map of what
  to write differently, and it stays in the audit.
- **policy, not eval.** the text says nothing of a test, a grader or pats. a model told it is being evaluated behaves
  as one that is.
- **the other responses are unchanged.** `404`, `reset` and `timeout` exist to look like the network, and a
  message would give them away.
- **on by default.** an eval that upgrades gets it with no change to its config, in a minor release whose notes
  name it. that changes what every eval's agents are told on a refusal, which is the point. `deny-message: ""` is
  the way back.

### `deny-message`

sets the body, for an eval whose prompt says something the default does not:

```yaml
egress:
  deny-message: |
    this environment has no access to that source.
```

- it is the sandbox's, as `deny-response` is: a task cannot set it. beside a `deny-response` other than `403` it is
  a load error.
- it is text the agent reads, as much a part of what the eval tells it as the prompt, and to be written with the
  same care: nothing in it that the agent should not know.
- valid utf-8, at most 2048 bytes, no control character but a newline. a `null` is a load error. the body is the
  text as written, with a final newline added when it has none, and `Content-Length` counts those bytes. the reason
  phrase and the headers are constants: nothing from the config is written into them.
- `deny-message: ""` is the refusal as it is today, by today's code path: `http.Error`, so `403 Forbidden`,
  `egress denied` and a connection the server may keep. it is there for a run that must compare with an older one.
- absent and empty differ, so the field keeps its presence wherever a policy is read or written. pats resolves an
  absent one to the default text when it loads the policy, and the policy it hands the proxy and records in the
  pair's `metadata.json` always carries `deny_message`: the text an agent was shown, or `""`. a policy file given to
  `pats egress run` is resolved the same way.

### where it applies

- a refused CONNECT, audited as an `open`.
- a refused request on a terminated connection or over plain http, audited as a `req`, whatever refused it: a
  `deny`, the default, a path no `allow` admits, the methods posture, a `Host` that names another host than the
  CONNECT did, a request with no path to decide on.
- not a key misuse (`key-proxy.md`), which keeps its own body: that is the harness's wiring, not the agent's choice.
- not a tunnel refused after its `200`: a hello naming another host, one that is not tls, a dial the guard refused.
  the connection closes, as now, since the answer to the CONNECT is already on the wire.
- not a `502`: an upstream failure is a fault, and says `egress: <reason>` as now.

the audit line is unchanged: a refusal is `status: 403` with its `rule` and `reason`. the status is the answer the
proxy decided on, whether or not the client stayed to read it.

## mechanics

go's `net/http` writes the status text itself, so a refusal with a phrase of its own is written by hand. in order:

1. the refusal is audited, before anything that can block.
2. the connection is hijacked. the handler owns it from here to its close, on a terminated connection too, where
   `serveConn` returns at the hijack as it does for a `reset`. what it writes goes through the connection the hijack
   returned, which is the tls one there.
3. the response is written, under a write deadline. nothing is read first, so a client waiting on
   `Expect: 100-continue` gets its final answer and no `100`.
4. the write side is closed, and what the client is still sending is discarded up to 64k or one second, whichever
   comes first.
5. the connection is closed.

- **best effort.** a client that sends more than step 4 takes may read a reset where the 403 was. the bound is
  what keeps a refusal from holding a connection open for a body nobody wants.
- **one connection for each refusal.** today's 403 usually leaves the connection open. requests a client pipelined
  behind a refused one are dropped with it, and the client sends them again on a new connection.
- **`HEAD`** gets the same headers, `Content-Length` included, and no body.
- **no hijack to be had** falls back to `http.Error`, as `reset` does.
- **the answer is `HTTP/1.1`** whatever the request was. no client speaks http/2 to the proxy (the CONNECT is
  http/1, and a terminated connection offers nothing else), so there is always a reason phrase to set.

## not doing

- **the entry or the reason in the message.** see above.
- **terminating tls on a refused host to say no inside it**, which would reach curl and git where a refused CONNECT
  does not. it needs the client to trust the run CA, which a policy that terminates nothing never installs, and a
  client that does not trust it sees a certificate error: a fault, and the opposite of the point.
- **a `Proxy-Status` header** (rfc 9209). right in principle, and shown by no client an agent uses.
- **the prompt.** what an agent is told before it starts is the eval's to write, and says more than a refusal can.
  an eval that wants attempts to cost something can score them: `blocked_egress` and `denied_egress` in
  `metadata.json` list the policy's refusals.

## open question

- **the reason phrase with a `deny-message`.** it stays the default's, which python and wget show alone. an eval
  whose message contradicts it has no way to say so there.

## tests

- on the wire, for a refused CONNECT, a terminated request and a plain one: the status line, the headers, the body,
  the connection closed after. the same over an http/1.0 request.
- the audit line is written when the client never reads the answer, and when the write fails.
- `deny-message` absent, set and `""`, each from pats.yaml through the policy the proxy is given to
  `metadata.json`. a `null`, a control character, bytes that are no utf-8 and 2049 bytes are load errors, and so is
  a `deny-message` beside `deny-response: 404`. a multi-byte message's `Content-Length` is its bytes.
- `404`, `reset` and `timeout` answer as they do now.
- a refused `POST`: a body under the bound reads the 403, a sender that stalls is closed within the bound, and one
  waiting on `Expect: 100-continue` reads the 403 without sending its body.
- a refusal after an allowed request on the same connection, and a request pipelined behind a refusal.
- a refused `HEAD` reads the headers and no body.
- a terminated connection's handler has returned and its connection is closed once the refusal is read.
- a key misuse and a `502` keep their bodies.
- spread, in a sandbox, for a refused url on a terminated host: `curl` and `git ls-remote` print the body, python
  prints the reason phrase.
