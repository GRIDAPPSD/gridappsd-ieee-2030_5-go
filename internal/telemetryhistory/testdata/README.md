# testdata: decoder fixtures

## real_capture_diff_message.json

CAPTURED, not synthetic. This is the exact `message` body of the one
difference-message frame observed on a live GridAPPS-D simulation input
topic during phase 2 of the coupling investigation, extracted verbatim
(only re-indented) from the `body` field of line 4 of
`artifacts/outputs/gago-094-phase2-bus-capture.jsonl` in the
gridappsd-ieee-2030_5-go knowledge project.

It carries `multiplier: 0` on both its forward and reverse difference.
Since `10^0 == 1`, this is the one multiplier value at which a decoder
that ignores the multiplier field entirely produces the SAME result as
one that applies it correctly. This fixture proves the decoder accepts
the real wire shape; it CANNOT, by itself, prove the multiplier is
applied at all. See the two synthetic fixtures below, which exist
specifically to close that gap.

## synthetic_multiplier_pos2.json and synthetic_multiplier_neg2.json

SYNTHETIC. Hand-built by copying the real capture's envelope shape and
changing only the `multiplier` field, to `2` and `-2` respectively, and
the `difference_mrid` (to a fixed, obviously-fake UUID so nobody mistakes
one for a real captured `difference_mrid`). No frame with a non-zero
multiplier has ever been observed on this bus; these two exist because
the real capture cannot exercise the scaling defect described above, and
a decoder must be proven correct in both scaling directions (a
positive-only test would still pass an implementation with the
multiplier's sign backwards).

Do NOT "correct" these back toward `multiplier: 0` to make them look more
like the real capture. That would remove the only test coverage this
package has for applying the multiplier at all. If a genuine non-zero-
multiplier frame is ever captured from a live platform, add it alongside
these, not in place of them.
