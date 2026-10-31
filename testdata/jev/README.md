# Jev Decisions fixtures

These files are hand-written fixtures. They follow the documented shapes of the
Decisions API (`POST /api/alpha/decisions`: `model`, `state`, `questions` in the
request; `answers`, `model`, `usage` in the response). No file here was
captured from a live call, and no value in them is a measurement.

Request fixtures are compared byte for byte, after `json.Compact`, with what the
client serializes:

- `request-structure-r1.json`: two tests, privacy level `structure`, rubric `r1`.
- `request-structure-r1s.json`: the same input with the short rubric form `r1s`.
- `request-none-text.json`: privacy level `none` with `send_hunk_text`, which adds
  the hunk text, the test purposes and the assertion targets.

Response fixtures answer the tests of `request-structure-r1.json`:

- `response-ok.json`: both questions answered, a dated snapshot of the configured
  release, `usage.cost` present.
- `response-partial.json`: `t_002` is missing and `t_001` is out of range.
- `response-drift.json`: the served model is a different release.
- `response-no-cost.json`: both questions answered, `usage` carries no cost.
- `response-tier.json`: a `choice` answer for the optional tier question.
