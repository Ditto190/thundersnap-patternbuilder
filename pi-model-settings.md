# Overriding GLM-5.2 `contextWindow` / `maxTokens` in pi

## The symptom

If you run pi with `defaultProvider: "aperture"` and `defaultModel: "glm-5.2"`
(via the `@aliou/pi-ts-aperture` extension), two things go wrong that both
trace to one root cause:

1. **Auto-compaction fires way too early** — around ~111K tokens, nowhere near
   the model's real ~1M context.
2. **Responses get truncated** — `maxTokens` is reported as `8192`, which is
   far too small for a coding model running with `defaultThinkingLevel: "high"`.

## Root cause

The Aperture extension hardcodes both numbers for *every* model it registers.
There is no per-model metadata; the gateway's `/v1/models` endpoint does not
serve `context_window` or `max_tokens` fields, so the extension falls back to
a fixed default.

The offending code, in
`~/.pi/agent/npm/node_modules/@aliou/pi-ts-aperture/extensions/aperture/dedicated/model-defaults.ts`:

```ts
return {
  id,
  name: id,
  reasoning: false,
  input: ["text"],
  cost,
  contextWindow: 128_000,   // ← hardcoded for every model
  maxTokens: 8_192,         // ← hardcoded for every model
};
```

`buildModels` in `dedicated/runtime.ts` calls this for every `modelId` in
every gateway provider. So pi believes `glm-5.2` has a 128K window and an 8K
output ceiling no matter what the model actually supports.

### Why `maxTokens: 8192` is too small

`maxTokens` is the cap on **output tokens per response** (completion + any
thinking/reasoning tokens the provider counts against the budget). It is not
the context window. With `defaultThinkingLevel: "high"`, a single nontrivial
coding step can easily emit 10–30K tokens of reasoning plus tool-call text;
8K means the model gets cut off mid-sequence and the turn aborts.

GLM-5.2's real output ceiling is **128K** (Zhipu ships it that way; their own
long-horizon evals run with `max_new_tokens` up to 131072). So `8192` is not
"conservative" — it is ~16× smaller than the model supports and small enough
to break normal coding-agent turns.

## The real GLM-5.2 numbers

From Zhipu's GLM-5.2 announcement (`https://z.ai/blog/glm-5.2`):

- **Context window: 1,000,000 tokens** ("a solid 1M-token context that stably
  sustains long-horizon work").
- **Max output: 128,000 tokens** (their FrontierSWE / PostTrainBench / SWE-Marathon
  evals all run with "128K maximum output tokens").

## The fix: `~/.pi/agent/models.json`

pi exposes a per-user, per-model override seam that is applied **after** the
extension registers its models, so it wins over the hardcoded defaults. This
is the only thing that works today (the gateway doesn't serve the fields yet).

Create or edit `~/.pi/agent/models.json`:

```json
{
  "providers": {
    "aperture": {
      "modelOverrides": {
        "glm-5.2": {
          "contextWindow": 1000000,
          "maxTokens": 131072
        }
      }
    }
  }
}
```

| Field | Value | Why |
|-------|-------|-----|
| `contextWindow` | `1000000` | GLM-5.2's real 1M-token window. Setting it higher than the model supports is safe — the provider just rejects oversized requests, failing loud. |
| `maxTokens` | `131072` | GLM-5.2's real 128K output ceiling. This is a *cap*, not a target — the model won't pad to it. Safe to set to the real max. |

### How to apply it

The file reloads every time the `/model` picker opens. So:

1. Save `~/.pi/agent/models.json` (above).
2. Open `/model` in pi.
3. Re-select `glm-5.2` (this is what picks up the new metadata).
4. Exit `/model`.

No restart is strictly required, but restarting pi is a clean way to be sure
the override took effect everywhere.

### How to verify it took

In pi, the `/model` picker shows per-model detail. After re-selecting, the
detail line for `glm-5.2` should reflect the larger window, not the 128K
default. The most reliable behavioral check: keep a long session going and
confirm auto-compaction no longer fires around ~111K tokens (it should now
fire around ~984K — see below).

## How this interacts with compaction

pi auto-compacts when:

```
contextTokens > contextWindow - reserveTokens
```

`reserveTokens` defaults to `16384` (`~/.pi/agent/settings.json`,
`compaction.reserveTokens`).

- **Before the override** (window = 128K): compaction fires at
  `128000 - 16384` ≈ **111K tokens**. This is why long sessions were getting
  compacted far short of the model's real capacity.
- **After the override** (window = 1M): compaction fires at
  `1000000 - 16384` ≈ **984K tokens**. A normal session basically never hits
  it.

Two compaction behaviors worth knowing (from pi's `agent-session.js`):

- **Threshold compaction** (context crossed the line after a *successful*
  response): pi compacts and then **stops and waits**. It does not auto-resume,
  because the turn already completed. Send any message ("continue", or the next
  instruction) to resume. The compaction summary preserves Goal / Progress /
  Key Decisions / Critical Context plus the cumulative read-files / modified-files
  list, so the agent has what it needs to pick up.
- **Overflow compaction** (the provider returned a context-length *error* mid-turn):
  pi compacts and **auto-retries** the aborted turn. This is the "continue past
  compaction" path and it is automatic; no config needed.

## Optional: keep more recent context across a compaction

When a threshold compaction *does* eventually fire (now only near 1M), the
default `keepRecentTokens` of `20000` is tiny relative to a 1M window — only
the last ~20K of reads/edits/tool calls survives the summary. To keep more
live working context across a compact, add a `compaction` block to
`~/.pi/agent/settings.json`:

```json
"compaction": {
  "enabled": true,
  "reserveTokens": 16384,
  "keepRecentTokens": 80000
}
```

This is optional and independent of the window fix. Leave `enabled: true`
(disabling it just pushes you into hard provider-limit rejections instead of
graceful summarization).

## The real fix (for context)

The override above is a user-side workaround. The underlying bug is that the
Aperture gateway's `/v1/models` endpoint does not serve `context_window` or
`max_tokens` fields, and the extension hardcodes a default instead of
deriving them per-model. For comparison, Vercel's AI Gateway `GET /v1/models`
returns both fields directly on each entry (`context_window`, `max_tokens`),
which is the shape pi's own `modelFromJson` already understands. The clean
fix is for the aperture gateway to serve those two fields and for the
extension to pass them through in `buildDefaultModelConfig` — after which the
`models.json` override becomes unnecessary for models the gateway knows about.
