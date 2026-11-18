# egress exceptions

status: implemented, for 1.3.0. extends the policy grammar in `docs/egress.md`.

## problem

a policy cannot say "all of this but that". a deny always beats an allow, so a path left open inside a denied tree
has to be spelled as every path around it.

the case that asked for it: a repository holds an eval's answers, and its `info/refs` must stay open for a setup
step's `git ls-remote`. as globs that is a deny for each thing git and the web serve under `<repo>.git/`, and
whatever the list misses is open. exactly, it is a regexp that spells "anything but `info/refs`" one letter at a
time, since go's regexps have no lookahead:

```yaml
deny:
  - 'github.com/*/answers.git/*'
  - 'github.com ~^/[^/]+/answers\.git/(?:[^i]|i[^n]|in[^f]|inf[^o]|info[^/]|info/(?:[^r]|r[^e]|re[^f]|ref[^s]|refs[^?]|(?:r|re|ref)?$))'
```

the pair does what it says (the glob takes the one-segment paths the regexp is too short for). nobody reads it, and
nobody edits it right.

the other way round is milder and the same shape: an allow for a tree but for one corner of it is an allow and a
deny some lines apart, and the deny reaches further than the allow it was written for.

## proposal

an entry in `allow` or in `deny` may be a mapping: the entry, and the requests it leaves alone.

```yaml
allow:
  - .ubuntu.com                       # a line, as now
  - match: 'github.com/canonical/**'
    except:
      - '/canonical/answers**'
deny:
  - 'github.com/*/answers'
  - 'github.com/*/answers/**'
  - match: 'github.com/*/answers.git**'
    except:
      - 'GET /*/answers.git/info/refs?service=git-upload-pack'
```

- **`match`** is an entry as a line writes it, method prefix and all. it must be a url entry: an exception is about
  paths, and a bare host is decided before any path is seen. `match: github.com` with an `except` is a load error
  that says to write `github.com/**`.
- **`except`** is a list of url parts, a glob (`/...`) or a regexp (`~...`), each with an optional method prefix.
  an item names no host: it applies wherever `match` does, which for a suffix entry is the name and every
  subdomain, and for an entry with no port is 80 and 443.

a deny entry takes a request when its `match` does and none of its `except` spares it. an allow entry admits a
request when its `match` does and none of its `except` withholds it.

- **an exception decides nothing.** a request a deny's `except` spares goes on as if the entry were not there: to
  the other deny entries, then the allows, then the default. a request an allow's `except` withholds goes on to the
  other allow entries, and is refused where none admits it.
- **it belongs to one entry.** it does not reach another, on any host. a task's entries and a harness's are unioned
  over the sandbox's as now, so neither can open a path the sandbox denies.
- **precedence is unchanged.** a deny match wins, then an allow, then the default. nothing in `allow` can weaken a
  deny, however it is written or wherever it sits.
- **an allow's `except` is not a deny.** it narrows one allow. another allow on the host, a bare one or the
  harness's among them, admits what it withholds. what must be out of reach whoever allows it is a deny.

## how an exception matches

an exception errs toward closed, which is the other way to the entry it sits on. a deny's is held tighter than an
allow is, and an allow's is read as a deny is.

### on a deny

`match` takes a request when any reading of its path matches. an `except`:

- **every reading.** it spares a request only when every reading of its path matches. so
  `/x/answers.git/info/refs/../../git-upload-pack` matches the entry and is not spared: it resolves to a path that
  is not `info/refs`. the readings are those of `docs/egress.md`: the path as sent, resolved, and as an origin reads
  it that drops `;parameters`, takes a backslash for a slash, folds a look-alike or decodes twice.
- **and each without its trailing slash**, as a deny is tried against the path. `/a/b/..` resolves to `/a/`, which
  an origin may serve as `/a`, so `/a/**` spares neither it nor `/a/`. an item that means the directory too is
  `/a**`.
- **no reading is waived.** an allow that itself writes a `;` or a backslash means the path as sent, and is let off
  the reading that rewrites it. an `except` is not: that waiver would spare `/a;b/key` where an origin that drops
  parameters serves `/a/key`, a path the entry denies. so a glob item that writes a `;`, a backslash, a `%` or a
  look-alike of a slash or a dot could never spare its own path, and is a load error. a regexp item that names one
  (`[^/;]+`) is left alone: it spares a path only when every reading of it matches, like any other.
- **the whole path.** a regexp item is anchored for it, as `^(?:...)$`. an entry's regexp is unanchored unless it
  says so, and an exception read that way would spare every path that holds the text.
- **case.** it matches case-sensitively. `match` does not, so `/x/answers.git/INFO/refs` is denied. a regexp that
  writes `(?i)` has asked for the other.
- **the query.** no wildcard takes the `?` before the query. an item that spells one, as the example does, spares
  the path with that query only. one that spells none spares the path with any. a path that holds a `?` of its own,
  sent as `%3F`, is spared by nothing: it would read as the query an item spells.
- **the method.** an item with a method prefix spares those methods only. one with none spares every method `match`
  takes.
- **no text.** a path that is no text (bytes that are no utf-8, a control character) is spared by nothing.

### on an allow

`match` admits a request when every reading of its path matches. an `except` is a deny's url part in all but name:

- **any reading.** it withholds a request when any reading of its path matches, alone or with the query behind it.
  so `/canonical/x/../answers` is withheld by `/canonical/answers**`, and so is `/canonical/ANSWERS`: it matches
  case-insensitively.
- **anywhere in the path.** a regexp item is unanchored unless it anchors itself, as a deny entry's is. a glob
  matches the whole path, as everywhere.
- **the method.** an item with a method prefix withholds those methods only.
- **nothing is a load error for what it writes.** a `;`, a backslash or a `%` in an item is matched as written, on
  every reading.

a regexp item is thus read two ways, whole-path on a deny and anywhere on an allow. each is the closed way for its
side, and an item that anchors itself reads the same on both.

## the host stage

an exception is read where a path is: on a terminated connection, or over plain http. it cannot open a host.

- under `default: deny` a request a deny's `except` spares still needs an allow. a host with no allow entry is
  refused at its CONNECT, before any path.
- a host with allow url entries passes only the paths one of them admits, under either default. a spared path is
  held to that too, and a path an allow's `except` withholds is refused there, `reason: url-not-allowed`, unless
  another allow admits it.
- a bare deny entry on the host refuses its CONNECT, and no `except` on another entry is asked.
- a bare allow entry on the host admits every path on it, so an allow's `except` beside one withholds nothing.
  `pats egress check` shows which entry admitted.
- a mapping is a url entry, so its host is terminated, as a line's would be.
- a key bound for the pair (`key-proxy.md`) is admitted by an allow, and an allow never beats a deny: a bound
  endpoint a deny takes is reachable only through that entry's own `except`.

## shape and validation

- the mapping form is for `egress.allow` and `egress.deny` on a sandbox and on a task, and in a policy file for
  `pats egress run`. a harness's entries, in a machine file, stay lines.
- a mapping takes `match` and `except` and no other key. `match` is a string. `except` is a list of one or more
  strings. a missing, `null` or empty `except` is a load error, and says to write the entry as a line.
- yaml and json carry the same thing: a line for an entry with no `except`, `{match, except}` for one with. that
  holds for pats.yaml, a policy file, the policy handed to the proxy and the effective policy in `metadata.json`.
  the decoder for the mixed list refuses unknown keys itself.
- two entries are the same when their `match` and their `except`, in order, are the same strings. the union of a
  sandbox's list, a task's and a harness's drops repeats by that.
- two deny entries with one `match` and different `except` are two entries, and a request is spared only when both
  spare it. two such allow entries admit what either admits.

the proxy and pats are one version when pats starts the proxy from its own image. where another is named (an
`image:`, `PATS_LXD_PROXY_BIN`) an older proxy fails to parse a policy with a mapping and does not start: closed.

## audit and check

- the audit does not change. a spared request shows the entry that then decided, an allow or `default`. a request
  a deny entry takes shows `rule: <match>`, as a line would. a withheld request another allow admits shows that
  allow, and one none admits shows `rule: <match>` of the first allow on the host that did not admit it, with
  `reason: url-not-allowed`, as now. two entries with one `match` are not told apart.
- `pats egress check` lists, before the verdict, each entry whose `match` matched and whose `except` then spared
  or withheld the request, in list order, with the item that did. without that a reader of the policy sees an
  entry that matches and a verdict that disagrees. the verdict line is as now, whatever decided: a later deny, an
  allow, the methods posture, the default.

## room for a programmatic matcher

a later version may let an entry carry a starlark predicate, for what a glob or a regexp cannot say.
`egress-when.md` is that design. this spec keeps three things true, so that it can be added without changing
anything written here:

- **the host stage runs no code.** it decides a CONNECT, and which hosts to terminate tls for, before there is a
  request to ask a predicate about. so an entry's host comes from its `match`, and an entry with none is one that
  has every host terminated, as `methods` does.
- **one reading at a time.** an entry takes or admits by its readings, and an `except` spares or withholds by
  them, whatever does the matching. a predicate is asked about one reading, a third kind of pattern beside the
  glob and the regexp, and is held to the same rule on each side. like a regexp item it is left to say what it
  means, with no check at load of what it writes.
- **what is not specified is a load error.** an unknown key on a mapping, a mapping with no `match` or no
  `except`, an item that is not a string, an item that starts with neither `/` nor `~` after its methods. each is
  a place a later version can give a meaning to, and a pats or a proxy older than that version refuses the policy
  where it would otherwise read less into it than was written.

## not doing

- **an allow that outranks a deny.** the same power with the hole written anywhere: an allow added later, for
  another reason, opens a path a deny forty lines away was written to close, and "a deny always wins" stops being
  true at a glance.
- **`!` lines in a list**, as gitignore has them. no new shape, but the order of the list starts to matter,
  and a `!` far below its entry is the allow-outranks-deny problem inside one list. binding a `!` to the line above
  it fixes that and makes a list whose lines cannot be reordered, merged or deduplicated one at a time, which the
  union of sandbox, task and harness lists does.
- **a negated segment in globs** (`/!info/**`). it reads well and needs four lines for a two-segment path, and the
  glob grammar gains a character every existing entry must not have meant.
- **another regexp engine, for lookahead.** the ones go has backtrack, in the path of every request, and the
  rewrite that keeps a wildcard from taking the `?` (`spelledQuery`) leans on re2's syntax tree.
- **exceptions to an exception.** one level. a policy that wants more is two entries.

## tests

- the deny case above: spared with its query and method, refused with another query, with `POST`, and under
  `default: deny` with no allow for the host.
- spared by `except`, taken by another deny entry. two deny entries with one `match`: spared only by both.
- every reading, on a deny: dot segments below the spared path, `..;/`, a backslash, `%2f` sent encoded twice, a
  doubled slash, a look-alike dot, each alone and together.
- a deny's regexp item is anchored: one that would match inside a longer path does not spare it.
- a path that ends in a slash, or resolves to one that does, is not spared by an item for what is below it. a path
  with a `?` of its own is not spared by an item that spells that query.
- case, on a deny: the spared path in another case is denied.
- the allow case above: admitted outside the item, withheld on it, through dot segments, `..;/`, a backslash, an
  encoded slash and another case. an unanchored regexp item withholds a longer path.
- an allow's withheld path is admitted by another url allow and by a bare allow on the host, and refused
  `url-not-allowed` with neither, under both defaults. two allow entries with one `match` admit what either does.
- a method prefix on `match`, on an item, and on both, on each side. method case.
- scope: a suffix `match` on the name and a subdomain, an ip, an explicit port, 80 and 443.
- the host stage: a CONNECT and a plain request under both defaults, with a bare allow, a url allow that admits the
  spared path, one that does not, and a bare deny.
- a spared request refused by the methods posture. a bound key's endpoint under a deny, spared and not.
- load errors: a bare-host `match`, a missing, `null` and empty `except`, an item with a host, an item that starts
  with neither `/` nor `~`, a deny's glob item that writes `;`, a backslash or `%`, a non-string item, an unknown
  key, a mapping in a machine file.
- a task's `except` and a harness's line do not spare a path the sandbox's deny takes.
- round trip, yaml and json: a mixed list from pats.yaml to the proxy's policy and `metadata.json`, an entry with no
  `except` a line in each.
- `pats egress check` names the entry and the item, with one spared entry and with two, and with a withheld one.
