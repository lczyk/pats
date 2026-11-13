# parse: a harness's output read by a script

status: implemented. it fills the `parse` key of a harness entry (`machine-file.md`, `harness-registry.md`) and
changes `budget.md` (the load error for `budget` becomes a warning), `agent-scorers.md` (a judge's reply is read
by the script) and `run-dry-run.md` (a harness with no cost reads as no history).

## problem

- **reading a harness's output is go, chosen by the harness's name.** three functions per format: the live scan
  (`statScanner`, `internal/eval/progress.go`), the summary (`summarize`) and a judge's final reply (`finalTexts`,
  `verdict.go`). each is the same loop: decode a line, switch on its type, count a tool, add some numbers, keep a
  text.
- **a vendor's format moves more often than anything else pats knows about it.** the code says so where it
  branches (`NOTE: a stream-format change breaks here`). the fix is a pats release.
- **a harness from a config has no reader.** it gets no tool count, no cost and its whole stdout as a judge's
  reply, whatever it prints.
- **a reader that goes wrong goes quiet.** a field the vendor renamed decodes to zero, and the column shows `0`.

## proposal

a harness entry's `parse` is a starlark script that pats runs once for each line of the harness's stdout. the
script says what the line means by calling a few functions pats gives it. the four readers become four scripts in
the defaults, and no go code reads a format.

```yaml
opencode-openrouter:
  parse: |
    ev = json.decode(line, default = None)
    if type(ev) != "dict":
        ev = {}
    t = ev.get("type")
    part = ev.get("part", {})
    if t == "tool_use":
        out.tool(part.get("tool", ""))
    elif t == "step_start":
        out.clear()
    elif t == "text":
        out.text(part.get("text", ""))
    elif t == "step_finish":
        out.clear(next = True)
        tok = part.get("tokens", {})
        cache = tok.get("cache", {})
        reasoning = tok.get("reasoning", 0)
        out.usage(
            turns = 1,
            cost_usd = part.get("cost", 0),
            input_tokens = tok.get("input", 0),
            output_tokens = tok.get("output", 0) + reasoning,
            reasoning_tokens = reasoning,
            cache_read_tokens = cache.get("read", 0),
            cache_write_tokens = cache.get("write", 0),
        )
    elif t == "error":
        err = ev.get("error", {})
        out.error(msg = err.get("data", {}).get("message") or err.get("name") or "")
```

the script is the body, as a slice's `mutate` is in chisel: it defines nothing pats then calls. what it needs is
handed to it by name.

### what the script gets

- **`line`** is one line of stdout as a string, in order, without its line ending: a `\n`, and one `\r` before
  it, in both passes. stderr is not read, as now. `stdout.log` keeps every byte.
- **`out`** is where the script says what it found (below).
- **`json`** is starlark's own module: `json.decode(line, default = None)` gives the default for a line that is
  not json, which is how a script skips one, since starlark has no way to catch an error.
- **`re`** is pats's: `re.search(pattern, s)` gives `None` or a tuple of the whole match and its groups, and
  `re.findall(pattern, s)` a list of them. the syntax and the engine are go's `regexp`: no lookaround and no
  backreferences, and time linear in the line whatever an agent printed into it. a pattern is compiled once.
- **nothing else.** no file, no network, no environment, no clock. starlark has no unbounded loop and no recursion
  unless a host allows them, and pats does not. each run has a budget of a million steps.
- **no state of its own.** every line is a run of the whole script from its top, so nothing a script sets lasts to
  the next line. what the four formats need across lines is in `out`'s rules below.
- a script may `def` helpers and call them, and may use `if` and `for` at its top level and set a name twice: pats
  turns on the two starlark options that takes. there is no `return` at the top level, so a script that skips a
  line wraps its body in an `if`.

### what the script says

| call | what it does |
|---|---|
| `out.tool(name)` | one tool call: the live count, and `tools[name]` in the summary |
| `out.usage(...)` | adds its keywords to the totals, or with `total = True` replaces those totals |
| `out.error(v = True, msg = "")` | sets `is_error`, and `error`: what the harness said of it |
| `out.text(s, sep = "\n")` | a piece of the reply, joined to the one before it by `sep` |
| `out.clear(next = False)` | drops the pieces so far, or with `next = True` once another arrives |
| `out.final(s = None, add = False)` | the reply, whole: it replaces the candidates before it, or joins them. with no `s`, the text so far |

the rules:

- **`usage`** takes `turns`, `cost_usd`, `input_tokens`, `output_tokens`, `reasoning_tokens`, `cache_read_tokens`,
  `cache_write_tokens`, `premium_requests`, `ttft_ms` and `api_duration_ms`, each optional: the names the summary
  in `metadata.json` has. `total = True` replaces the totals of the keywords given and leaves the others: for a
  format that restates its totals, and for the two times, which are stated once.
- **`error`** is the last call's: `out.error(False)` takes an error back, and its message with it. the message is
  optional, and is what a failed preflight and a judge's error show in place of the end of the output.
- **a number** is finite and not negative, and all but `cost_usd` are whole. a call with one that is not, or whose
  total would no longer be finite or fit the summary's field, is an error and changes nothing: a negative cost
  would take spend back, and a `nan` or an infinity cannot be written to `metadata.json`.
- **the totals mean what `runSummary` says they mean**, for every harness: `input_tokens` leaves out
  cache reads, `output_tokens` includes `reasoning_tokens`, `turns` are model round trips. the script does the arithmetic its format needs.
- **`text`** pieces join by newlines, which suits a format that prints a part at a time. `sep` joins a piece to
  the one before it by something else: `sep = ""` for a reply that streams a token an event.
- **`final()`** with no reply keeps the text so far as a candidate and spends it, so what `text` hears next is no
  part of it: for a text format whose reply is the lines between two markers. with no text so far it does
  nothing.
- **`has_cost`** is true once `cost_usd` was passed at all, 0 included: a real zero, not an unreported one.
- **the reply** a judge is read from (`agent-scorers.md`) is the `final` candidates, newest first, when there are
  any, and else the pieces of `text` joined by newlines. with neither there is no reply.
- **no summary** when the script made no call but `text`, `clear` and `final`, and no line failed: an empty or
  foreign log, as now. a failed line is enough for one, so `parse_errors` always has a place.
- **a harness with no `parse`** has no summary and no tool count, and its whole stdout is its reply.

### when it runs

twice over the same lines, as the go readers do now, each time from a fresh `out`:

- **as the pair runs**, on the tee: the tool count in the progress line, and the cost `budget` and `--budget`
  are checked against.
- **after it**, over `stdout.log`: the summary in `metadata.json` and a judge's reply. so both are a function of the
  log and the script, and can be made again from them.

the two frame lines as the two readers do now, and differ in two cases. the live pass cuts a line at 8 MiB and
never sees a last line with no newline. the pass after reads that last line, and a line over 8 MiB fails its whole
read: no summary, and for a judge an error, not an earlier reply. outside those two the passes see the same lines
and reach the same totals.

the script runs in pats, on the host, under every driver. `stdout.log` stays what the harness printed.

### when it fails

- **at load:** a script that does not compile is a load error naming the harness and the script's line: a syntax
  error, or a name that is neither the script's own nor one of the four handed in. so is a `parse` in the
  defaults: a test compiles each.
- **on a line:** an error in a run (a key that is not there, a wrong type, a bad pattern, the step budget) ends
  that run and nothing else. what the run said before it failed stands. pats counts them, warns once per pair
  with the first one (`WARN <pair>: parse: line 412: key "cost" not in dict (3 lines failed)`), and the summary
  carries `parse_errors`.
- **for a judge** a failed line fails the sample: its verdict is an error, whatever reply was read. the line that
  failed may be the one that took a score back or reported the harness's own error, and an earlier candidate is
  not to be trusted over it. a task's pair keeps its partial summary.
- that is what indexing a field buys (`part["cost"]`), where `.get` with a default reads a missing one as zero: a
  vendor that renames the field shows up as a warning and a count. the four defaults index nothing yet. they read
  what the go readers read, a missing field as zero, until recorded runs say which fields a format always has: a
  step whose tokens went missing must not lose its cost to an error.

### trying one

`pats harness parse <harness> [file]` runs a harness's script over a log (stdin without a file) and prints the
summary, the reply and each failed line. with a `pats.yaml` in reach it knows the project's harnesses too. it is
how a script is written against a recorded log, and how a changed format is diagnosed.

### cost, and `budget.md`

pats cannot know what a script will report, so nothing about cost is declared and nothing is checked at load:

- `budget` on an agent is accepted for every harness. it is checked on every report of cost while the pair
  runs, and a pair whose reported cost passes it is stopped and has the status `budget`, as now. a format that
  reports as it goes (opencode, per step) is stopped mid-run. one that reports once at its end (claude) is stopped
  there, with its work done: the cap then marks the pair and saves nothing.
- the load error of `budget.md` ("`budget` on an agent whose harness reports no live cost") goes, with
  `LiveCostHarnesses` and `noCostHarnesses`. one warning stands in for it: after preflight, for each agent whose
  preflight reported no cost and that has a `budget`, or is run under `--budget`: `WARN agent X reports no
  cost: its budget has nothing to check`. this is also the start-of-run warning `--budget` has now.
- the dry run's estimate stays what past runs' summaries say, and is unknown for an agent with none that has a
  cost.

a cap was a limit on reported spend and never a ceiling (`budget.md`). this widens the gap it admits: with
preflight skipped nothing warns, and a harness that reports no cost runs uncapped in silence.

### the four scripts

each is the go reader it replaces, line for line, and passes the tests that reader had. two things differ on
purpose. a negative number is an error here (above), where the go readers recorded it: a negative cost, which
they treated differently live and after, and a count the arithmetic takes below zero (codex's cached tokens above
its input). and cost reaches the live pass whenever the script reports it, where the go reader of claude kept it
for the summary. what each has to keep:

- **claude.** tools are the `tool_use` blocks of each `assistant` event's `message.content`. the totals, the
  times and `is_error` come on `result`, and a later `result` replaces an earlier one's: `usage(total = True)`.
  every `result` is a reply candidate (`final(..., add = True)`): a late note from a sub-agent follows the answer,
  and the verdict passes over one with no score in it.
- **codex.** tools are `item.completed` of type `command_execution`, `file_change`, `web_search`, and
  `mcp_tool_call` as `mcp:<server>/<tool>`. `turn.completed` carries usage, where cached tokens are a part of input
  and are taken out of it. no `turns` and no `cost_usd`: one turn by construction, and no price. `turn.failed` and
  `error` set the error. the reply is the last `agent_message`.
- **copilot.** tools on `tool.execution_start`, a turn on `assistant.turn_end`, output tokens and the reply (the
  last non-empty one) on `assistant.message`. `result` carries `premium_requests`, the api time and the exit code,
  which is the error when not 0.
- **opencode.** above. `step_start` clears the reply's pieces, and `step_finish` clears them once more text
  arrives: the reply is the text of the last step, and a step that ended stays the reply until a later one says
  something.

a fifth came after, with no go reader before it, written against a recorded run:

- **pi.** tools on `tool_execution_start`. each assistant `message_end` is a turn and carries its usage, its cost
  and its `stopReason`: reasoning is a part of its output, and cached tokens are none of its input. the reply is
  the text of the last assistant message that has any. `turn_end` and `agent_end` restate messages and are not
  read. pi exits 0 on a reply that failed, so `error` and `aborted` as the last message's stop reason are the
  error, with the message's `errorMessage` as what it says, and a retry that worked clears it. what a compaction's
  own call cost is not counted.

### an error with exit 0

preflight fails an agent whose harness exits 0 while its script has the error set at the end, as it fails a
non-zero exit: pi's json mode exits 0 on a refused key. either failure names what the script passed as the
message, and without one shows the end of the output. a pair is as before: its status is its exit, and the
summary's `is_error` keeps it from a judge.

of the five, four scripts pass a message, each checked against a run with a refused credential: pi's
`errorMessage`, the text of claude's failed `result`, the message of opencode's `error` (its name when it has
none), and the message of codex's `turn.failed` or `error`. copilot refuses a bad token on stderr before it
prints an event, so its script passes none and the end of the output is what shows.

## limits

- **cost is as good as the script.** a script that reports none leaves a cap with nothing to check, and says so
  only through the warnings above. the one case pats fills in: tokens with no cost, on a model of the openrouter
  catalogue, are priced at its list prices and marked `cost_estimated` (`budget.md`).
- **a script cannot keep state.** a format whose meaning needs more than the reply rules and the totals give is not
  readable yet. `out.state`, a dict, would be the additive way in.
- **the whole script runs per line.** constants and helpers at its top are made again each time: nothing next to
  decoding the line.
- **text formats are read with string methods and `re`.** go's regexp, not python's.
- **a project's script runs in pats**, and is trusted as the `pats.yaml` it is in. it can compute and nothing
  else, and the step budget bounds its steps per line, not its memory or its time: one call that builds a huge
  string is one step. a slow script slows the tee it runs on.
- **the calls are a promise** as the keys are: a script written for one pats is an error under one that renames a
  call.

## not doing

- **jq.** shorter, but a field that is not there is `null` and arithmetic on it goes through: the quiet zero this
  is meant to end. it would also have to be kept from the environment.
- **a table of paths per column.** the formats need a loop over an array, arithmetic and a built name within their
  first four.
- **an adapter inside the sandbox** (`run: cli | to-pats`). `stdout.log` would no longer be what the harness
  printed, and the adapter has to be in every image.
- **a usage file** a harness writes. a harness that knows its totals prints a line and its script reads it.
- **`cost` as a declared key.** a declaration pats cannot check, for a cap that is not a promise.

## tests

- each default's script against the logs the go reader was tested with: the same summary, the same live tool
  count, the same reply, and the live cost each format's lines give (claude's at its `result`).
- `out`: `usage` adds, and `total = True` replaces only what it names; `has_cost` after `cost_usd = 0`; `final`
  replaces, `add = True` keeps, newest first; `text`, `clear` and `clear(next = True)`; no summary with only reply
  calls.
- the two passes give the same totals, over `\n` and over `\r\n` lines; a last line with no newline counts
  after and not live; a line over 8 MiB
  is cut live, and after it there is no summary and a judge's verdict is an error.
- a judge's sample with one failed line has an error for a verdict, with a scored candidate before the line.
- failure: a script with a syntax error, and one that names something not handed in, at load; a missing key on
  one line, with the lines before and after counted; the step budget; a bad pattern; `parse_errors`
  and the one warning.
- `re`: a match with groups, none, `findall`; a pattern go's `regexp` refuses.
- hermetic: a script cannot name `load`, open a file or read the environment; a name set on one line is not there
  on the next.
- no `parse`: no summary, the whole stdout as a judge's reply.
- budget: `budget` on a claude agent loads, and a claude pair over it is `budget` at its `result`; the warning
  after a preflight with no cost; an opencode pair is still stopped mid-run.
- numbers: a negative, a `nan` and a fractional count are each an error that changes no total.
- `pats harness parse` over a recorded log prints the summary and the reply, and names a failed line.

## decided, open to change

- **starlark, with `json` and `re` handed in.** python to read, strict about what is not there, and closed by
  construction. one new dependency, `go.starlark.net`, pure go.
- **the script gets the raw line** and decodes it itself. a harness need not print json, and what is skipped is in
  the script, not in a rule of pats.
- **the script is the body, with no function to define.** nothing to declare and forget to call, and each line
  being a fresh run is plain to see.
- **calls, not a returned value.** a line can say several things, and the totals' rules (add, replace, newest
  first) are pats's, once, not each script's.
- **the readers move to scripts before the machine's file opens** (`harnesses-1x.md`), so `parse` never has a
  second meaning.
