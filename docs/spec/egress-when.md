# egress predicates

status: proposed, not scheduled. builds on `egress-except.md`, and is written now so that nothing 1.3.0 ships has to be
taken back for it. extends the policy grammar in `docs/egress.md`.

## problem

an entry's url part is a glob or a regexp over a path and its query. that cannot say:

- **anything about a header.** an entry cannot tell a `git` fetch from a browser's, or a request that carries a
  token from one that does not.
- **anything about a decoded query.** `/search?q=repo%3Ax%2Fanswers` names the answers, and a pattern over the raw
  query has to spell every encoding of it.
- **a list.** "one of these forty packages" is forty entries or one alternation nobody reads.
- **the same thing twice.** a test two entries share is written out in both.

each is a few words of code. pats already runs starlark for a harness's `parse` (`harness-parse.md`), so the
language, its limits and its dependency are in the tree.

## proposal

a mapping entry may carry `when`: a starlark expression asked about a request, true or false.

```yaml
egress:
  default: deny
  predicates-file: egress.star       # or `predicates: |`, the source itself
  allow:
    - github.com                     # a line, as now
    - match: 'pypi.org/simple/**'
      when: req.path.split("/")[2] in PACKAGES
  deny:
    - match: 'github.com/*/answers.git**'          # 1.3.0, as egress-except.md has it
      except:
        - 'GET /*/answers.git/info/refs?service=git-upload-pack'
    - match: 'api.github.com/search/**'            # a predicate narrows the match
      when: names_answers(req.params.get("q", ()))
    - when: answers_in_a_query(req)                # no match: every request seen in clear
```

```python
# egress.star
PACKAGES = ["requests", "numpy"]
ANSWERS = ["answers", "chisel-releases"]

def names_answers(values):
    return any([a in v.lower() for v in values for a in ANSWERS])

def answers_in_a_query(req):
    return any([names_answers(vs) for vs in req.params.values()])
```

- **`when`** is one expression. it sees `req`, `re`, and what `predicates` defines. it gives `True` or `False`.
- **`match`** is as in `egress-except.md`, and is now optional beside a `when`. an entry with both takes a request
  both take. an entry with a `when` and no `match` is asked about every request the proxy sees in clear (below).
- **`except`** items may be mappings too: `match`, a url part as a string item is, and `when`, one or both.
- **`allow` and `deny` both take it**, as both take an `except`.
- **precedence is unchanged.** a deny wins, then an allow, then the default. a predicate decides whether its entry
  matches, and nothing else.

## what a predicate sees

`req` is one request, frozen:

| field | what it is |
|---|---|
| `req.method` | the method, upper-cased |
| `req.scheme` | `https` on a terminated connection, `http` on a plain one |
| `req.host` | the host as the policy compares it: lower-case, idna ascii, no trailing dot, or an address |
| `req.port` | the port, a number |
| `req.path` | one reading of the path (below) |
| `req.query` | the query as sent, without its `?`, `""` for none |
| `req.params` | the query decoded: a dict from each name to a tuple of its values, in order |
| `req.headers` | a dict from each header name, lower-cased, to a tuple of its values, in order |

- **a repeat is a tuple**, in `params` and in `headers` alike. origins disagree on which of two counts, the first,
  the last or both joined, so a predicate is handed all of them and looks at each.
- **`params`** is decoded once, a `+` as a space. a query that does not decode (a bad escape, a `;` between pairs)
  has no `params`: reading the field is an error, with what an error means below. that closes an entry whose
  predicate reads it. it is not a refusal of the request, and a predicate that reads only `req.query` is not
  affected.
- **`headers`** are the request's as the client sent them and as go's server hands them over, before the proxy
  drops or rewrites any. `Host` and `Transfer-Encoding` are not among them: `req.host` is the host the policy
  decided on. trailers are not among them either. where the proxy holds a key (`key-proxy.md`) a predicate sees the
  placeholder, never the key, since a misused one is refused before the policy is asked.
- **no body.** see "not doing".
- **case is the predicate's.** nothing is folded for it: `req.path` and the rest are as sent. a deny's `match` is
  case-insensitive and its `when` is not, so a deny that compares text lower-cases it, as the example does.
- **`re`** is `re.search` and `re.findall` over go's `regexp`, as `harness-parse.md` describes them: time linear in
  the input. the proxy keeps its own cache of compiled patterns for a policy, and compiling one does not hold up
  another request. a pattern over 1 KiB is an error. a predicate that builds a pattern from the request compiles
  one on every call, so a pattern is written as a constant.
- **`predicates`** is a starlark file run once when the policy loads, under a budget of a million steps. what it
  defines at its top level, functions and values, is in scope for every `when`, frozen. it sees `re` and nothing
  else: no `req`, no file, no network, no environment, no clock, no `load`. it is plain starlark, without the two
  options a `parse` script turns on.
- **nothing lasts.** each call runs on a thread of its own over the frozen globals, and cannot keep a count or
  remember a request. a verdict is a function of the request and the policy.

## readings

a predicate is asked about one reading of a path at a time. the readings are the paths `match` is tried against
on that side today (`docs/egress.md`): the path as sent, the path resolved, and both again as an origin reads them
that drops `;parameters`, takes a backslash for a slash, folds a look-alike or decodes twice. at most thirteen on a
deny and twelve on an allow or an `except`, fewer where two come out the same.

- **a deny takes a request when one reading satisfies the entry.** the readings are asked in order. for each,
  `match` is tried against it alone and with the query behind it, as now, and where either matches, `when` is asked
  with `req.path` that reading. the first reading both hold of takes the request. `when` is never asked about a
  reading `match` did not take, so the two cannot be met by two different readings.
- **an allow admits a request when every reading satisfies the entry.** in order, for each: `match` must match it,
  or be waived for it as it is today for an entry that itself writes a `;` or a backslash, and then `when` must be
  true of it. `when` has no waiver: it is asked about a reading `match` was let off. the first reading that fails
  ends it, and the entry does not admit.
- **a deny's `except` item spares a request when every reading satisfies the item**, the same way and with no
  waiver, as in `egress-except.md`.
- **an allow's `except` item withholds a request when one reading satisfies the item**, as a deny takes one.
- **with no `match`** the `when` is asked about every reading.

so a predicate is written as if `req.path` were the one true path, and `/public/../private` is asked about as
`/private` too. a predicate that never looks at the path gives one answer for every reading.

- **a path that is no text** (bytes that are no utf-8, a control character) has no reading to ask about. a deny
  entry whose host and method cover it takes it unasked, no url allow admits it and no `except` spares it, as now.
- **the query has one reading.** it is not resolved or rewritten, and a predicate gets it apart from the path, so a
  query cannot stand in for one.

## errors

- **at load:** a `when` that is no expression, a syntax error, a name that is neither handed in nor defined in
  `predicates`, a `predicates` that fails when run, spends its budget or defines `req` or `re`, a `load`. each names
  the entry and the line. a typo in a field of `req` is not caught at load: starlark does not check attributes
  until they are read.
- **in a run:** an error, a result that is not `True` or `False`, or a budget spent. a function left uncalled
  (`when: names_answers`) is truthy in python and an error here, which is why the result must be a bool.
- **an error closes its entry, not the request.** each failure counts the closed way for where it sits. a deny's
  `when` and an allow's `except` item count as true: the entry takes the request, the item withholds it. an
  allow's `when` and a deny's `except` item count as false: the entry does not admit, the item does not spare. the
  verdict is then what the rest of the policy gives, so a request another allow admits is still admitted, and one
  another item spares is still spared.
- **the budget** is 100,000 steps for a call and a million for a request, across its entries and readings. past
  the second, every predicate still to be asked fails. it bounds steps and not memory or time: a builtin that
  builds a large value is one step. the script is the eval author's and is trusted as the rest of the config is.
  its input is the agent's and is not, and is bounded by the megabyte go's server allows a request head, on a
  terminated connection and a plain one alike. there is no limit by the clock: it would make a verdict depend on
  the machine's load.
- **what a failure leaves:** the request's audit line carries `when_error` whatever its verdict: the entry, as
  `rule` would name it, the place of the `except` item where it was one, and the first message in the order the
  predicates were asked. the message is cut to 200 bytes and has held keys masked, as the audit's other fields do.
  where the entry that failed is the one that refused the request, the line's `reason` is `when-error`. the pair's
  `metadata.json` counts them as `when_errors`, and the pair warns once with the first, as a failed `parse` line
  does.

## the host stage

a predicate is asked about requests, and a CONNECT is not one. so the host stage stays what it is, decided from
the entries' hosts alone:

- an entry with a `when` is a url entry: its host is terminated, and a `match` beside a `when` must have a url
  part. `match: github.com` with a `when` is a load error that says to write `github.com/**`.
- a deny with a `when` never refuses a CONNECT. an allow with a `when` passes the CONNECT of the hosts it covers,
  as an allow url entry does.
- the policy needs the run CA once any entry has a `when`.

### an entry with no `match`

it is asked about every request the proxy sees in clear, on any host, port and method: the predicate does all the
matching, `req.host` included. what the proxy sees in clear is what `methods` makes it see, and the same limits
hold:

- **every host but the harness's is inspected**, on every port. the proxy terminates tls on each connection it
  lets through, since a tunnel shows no request to ask about. under `default: allow` that is every host reached. a
  client that does not trust the run CA fails its handshake, and a connection that is not tls (an ssh session on
  22) is refused, `reason: not-tls`: closed.
- **the harness's hosts are not.** the hosts pats merged in for the harness stay tunnels, and what goes through
  them is not asked about. this is the one thing such an entry does not cover, and it is a way round a deny for
  whatever a harness host serves. a request seen on one anyway (a url entry names it, a bound key, plain http) is
  asked about. an eval that wants a harness host covered names it in a `match`.
- **a deny** takes any request seen in clear that its `when` is true of.
- **an allow** passes every CONNECT and admits the requests its `when` is true of. it is one allow among the
  others: a host a bare allow covers is still admitted whole, the harness's among them, and a bound key's endpoint
  by its own entry. with no other allow it is `default: deny` with the predicate as the allow list. under
  `default: allow` it is a load error, since it would restrict every host, which is `default: deny` said sideways.
- **the methods posture** still applies to what such an allow admits: it has no method prefix to override it with.
- inspection is by entry, not by need: a host a bare allow admits whole is terminated all the same, so that the
  rule for what is inspected stays one line.

## shape and validation

- **the entry.** a line, or a mapping of `match`, `when` and `except` and no other key. a mapping needs a `match`
  or a `when`. one that holds a `match` and nothing else is a load error that says to write the line.
- **an `except` item.** a string, or a mapping of `match` and `when` with at least one. a mapping that holds a
  `match` alone is a load error that says to write the string.
- **every value is a string with something in it**, `except` a list of one or more items. a `null`, an empty
  string, an empty list and a key written twice are load errors.
- **`predicates` and `predicates-file`** are the sandbox's, as `deny-response` is, and at most one is set. the file
  is read relative to the config, as `prompt-file` is, and must be utf-8.
- **a task's entries** may be mappings and may carry a `when`. they see the `predicates` of the sandbox they are
  paired with. every pair's policy is compiled when the config loads, before any pair runs, and a name a sandbox
  does not define is a load error that names the pair. a task sets no `predicates` of its own.
- **a harness's entries**, in a machine file, stay lines.
- **on the wire.** the policy handed to the proxy, a policy file for `pats egress run` and the effective policy in
  `metadata.json` carry the source as `predicates`, never the file's name, so a run records the code that decided
  it. a policy file may name a `predicates-file`, read relative to itself, and a policy given through
  `PATS_EGRESS_POLICY` may not. entries are lines and mappings as above, the same in yaml and json.
- **size.** the policy as handed to the proxy, source and entries together, is at most 96 KiB, a load error past
  it: it travels as one environment variable, and linux allows one 128.
- **two entries are the same** when their keys hold the same strings, `except` in order. the union of a sandbox's
  list, a task's and a harness's drops repeats by that.
- **an older proxy** (an `image:`, `PATS_LXD_PROXY_BIN`) fails to parse a `when` or a `predicates`, and does
  not start: closed. that rests on its decoder refusing what it does not know, which it does
  today, and on pats writing each of these only where a policy uses it.

## audit and check

- **`rule`** is the entry's `match` where it has one, as in `egress-except.md`. with none it is `when: ` and the
  expression, each run of whitespace in it written as one space. an expression that calls a helper is then its own
  name: `when: answers_in_a_query(req)`. two entries with one `match` are not told apart, as that spec has it.
- **`pats egress check`** takes `-H 'name: value'`, repeatable, since a predicate may read a header. before the
  verdict it lists each entry whose `when` was asked, what it said and on which reading, and any error. with an
  entry that has no `match` it says the host would be terminated for it, or that it is a harness host and would
  not be. it decides with the code the proxy does.
- **trying one** is `pats egress check`. there is no second tool.

## what this asks of `egress-except.md`

nothing in what 1.3.0 accepts or writes has to change. each step from it to this spec gives a meaning to something
1.3.0 refuses, in pats.yaml, in a task, in a policy file and in the policy the proxy is handed:

| 1.3.0 refuses | here it is |
|---|---|
| a mapping key other than `match` and `except` | `when` |
| a mapping with no `match` | an entry asked about every request seen in clear |
| a mapping with no `except` | an entry with a `when` |
| an `except` item that is not a string | a mapping of `match` and `when` |
| the keys `predicates` and `predicates-file` | the source |

so a 1.3.0 pats or proxy handed a policy from this spec refuses it, and never reads less into it than was
written. four things in how 1.3.0 is built keep the step small:

- **one decoder for an entry**, a line or a mapping, used for `allow` and `deny` both and in every place a policy
  is read. and one for an `except` item, refusing a mapping. each refuses unknown keys itself. the refusals are
  then the only thing to lift.
- **readings as values.** today a deny matches a list of candidate strings and an allow walks readings that carry
  what rewrote them. 1.3.0's `except` should ask its question of one reading at a time, a reading that keeps its
  path, its rewrites and whether it is with the query, so that a second question can later be asked of the same
  one. a matcher that answers "some candidate matched" cannot be extended to "`match` and `when` of the same
  reading".
- **an answer that can fail.** what is asked of a reading returns yes, no or an error, though a regexp never
  errors, and the entry that decided is kept with the verdict, not only its text.
- **the host stage asks an entry whether it inspects**, not whether it holds a regexp. today the two are the same.

## not doing

- **the body.** a predicate over it means holding a request until its body has arrived, a bound on how much, and an
  answer for a body that never ends. a graphql query is the case that wants it, and it gets a spec of its own.
- **the answer.** a predicate is asked before the request goes on, and never about what came back.
- **a predicate at the CONNECT.** asking with a host and no path means every predicate handles a request that is
  half there. an entry's host is its `match`, and an entry with none inspects.
- **a verdict from the predicate.** it says whether its entry matches. a function that returns allow or deny, or a
  message, would be a second precedence beside the lists.
- **a policy that is one function.** the lists stay: they are what `pats egress check`, `suggest` and a reader of
  the config go by.
- **an error that refuses the request.** it would let one entry's bug, or a query an agent bent to trip it, take
  down a request another entry admits, the harness's own among them.
- **a wire version.** the older decoder's refusal of what it does not know is the boundary, and is tested against
  a 1.3.0 build.
- **rewriting a request.** a predicate reads.
- **state.** a count of requests or a rate is not a function of the request.
- **`load`, and helpers of a task's own.** one file, the sandbox's.
- **another language.** cel and rego are built for this and are a second dependency and a second thing to learn.
  starlark is in the tree, closed by construction, and reads as python.

## decided, open to change

- **harness hosts are not inspected for an entry with no `match`**, as for `methods`. the other way errs closed and
  breaks a harness whose client pins its certificate or speaks a protocol a terminated connection does not pass.
- **no case folding for a predicate.** asking a deny's predicate about a lower-cased copy of the request as well
  was tried on paper. it misses a predicate that writes a capital in one comparison and none in another, it makes
  `req.params["Sig"]` an error on the copy and so a refusal, and it reads as a promise it does not keep.
- **the result must be a bool.** truthiness would let `when: re.search(...)` stand, and `when: helper` too.
- **a repeat is a tuple**, for headers as for the query, where a header's values joined by `, ` would read better.
- **no `name` for an entry.** the audit's `rule` is the `match` or the expression. a key that names an entry would
  tell apart two with one `match` and keep a scorer's `rule` steady when an expression is edited, and can be added
  later.
- **both `predicates` and `predicates-file`.** `prompt` and `prompt-file` are the precedent.

## tests

- a `when` on a deny, an allow and an `except` item of each, each true and false, with and without a `match`.
- readings: a predicate on the path asked about a dot segment, `..;/`, a backslash, `%2f` sent encoded twice and a
  look-alike, on each side. crossed readings: a `match` that holds of one reading only and a `when` that holds of
  another only, on a deny (not taken) and on an allow (not admitted), with a regexp that spells a query too. an
  allow's `match` waived for a `;` it writes and its `when` still asked.
- case: a deny's `when` that does not lower-case misses a capital, and one that does takes it.
- a path that is no text: taken by a deny with a `when`, unasked, and admitted by no url allow and spared by
  nothing.
- `params`: a name sent twice, a `+`, an escape, bytes that are no utf-8, a query that does not decode, on each
  side, and a predicate that short-circuits before reading it. `headers`: a name sent twice, case of the name,
  `Cookie` and `Authorization`, no `host`, a placeholder where a key is held, the same on a terminated connection
  and a plain one.
- errors: a runtime error, a result that is no bool, a function left uncalled, each budget, on each side. an
  allow's error beside a bare allow that admits, and an item's error beside an item that spares: the verdict
  stands and `when_error` is on the line. a message that holds a placeholder is masked, and a long one cut.
  `when_errors` in the metadata and one warning.
- load errors: a syntax error in a `when` and in `predicates`, an undefined name, a `load`, a statement where an
  expression goes, a `predicates` that spends its budget, `predicates` and `predicates-file` together, a policy
  over 96 KiB, a `null` and an empty `when` and `match`, a key twice, a `match` alone in a mapping, a
  bare-host `match` beside a `when`, a `name` key, an allow with no `match` under
  `default: allow`, a task's `when` naming what its sandbox does not define, a `predicates-file` in a policy from
  the environment.
- the host stage: an entry with a `when` terminates its host. one with no `match` terminates every host let
  through but a harness's, on 443 and on another port, under both defaults, and refuses a connection that is not
  tls. a client without the CA fails closed. an allow with no `match` passes a CONNECT and admits by its predicate,
  under a methods posture too, and a bare allow beside it still admits its host whole.
- a request seen in clear on a harness host and on a bound key's endpoint is asked about, and one tunnelled to a
  harness host is not.
- the same request decided twice gives the same verdict. many at once share nothing and do not wait on each
  other's patterns.
- round trip, yaml and json: lines and mappings in `allow` and `deny` from pats.yaml to the proxy's policy and
  `metadata.json`, the source inlined from a `predicates-file`.
- a 1.3.0 build handed each row of the table above refuses to load it, as config and as a proxy.
- `pats egress check` with `-H`: the entries asked, the reading, an error, the note for an entry with no `match`,
  and the same verdict the proxy gives for the same request.
- a benchmark: a request against a policy with ten predicates, beside the same policy as globs.
