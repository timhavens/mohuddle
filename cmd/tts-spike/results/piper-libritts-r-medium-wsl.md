# Piper LibriTTS-R medium WSL spike

Date: 2026-08-22

Status: preferred Piper configuration for the remaining listening and
cancellation gates. This is not yet a provider-selection decision.

## Why this configuration was added

The initial Piper spike kept four single-speaker models warm. Three evaluated
voice models were CC BY-NC-SA and the fourth had unresolved terms, so that set
cannot support a redistributable default. It also overstated Piper's memory
requirement because Piper supports multi-speaker models.

`en_US-libritts_r-medium` contains 904 speakers in one model. Its model card
identifies the LibriTTS-R dataset license as CC BY 4.0, which permits
redistribution with attribution. Piper itself remains GPL-3.0-or-later, and the
initial MoHuddle posture remains invocation of a user-installed runtime rather
than bundling it.

If MoHuddle ever distributes or automatically downloads this model, the
LibriTTS-R attribution must be carried in an appropriate NOTICE/README entry.
User-directed installation remains the lighter initial posture.

Primary model card:

```text
https://huggingface.co/rhasspy/piper-voices/raw/main/en/en_US/libritts_r/medium/MODEL_CARD
```

Pinned artifacts:

| Artifact | Size | SHA-256 |
| --- | ---: | --- |
| `en_US-libritts_r-medium.onnx` | 75 MiB | `10bb85e071d616fcf4071f369f1799d0491492ab3c5d552ec19fb548fac13195` |
| Model configuration | 20 KiB | `b471dc60d2d8335e819c393d196d6fbf792817f40051257b269878505bc9afb3` |

The runtime, host, and corpus are otherwise identical to the initial Piper
result. These tables contain one run per case; repeat runs and audible review
remain required.

## Four-speaker mapping

The speaker labels are original LibriTTS speaker identifiers. Piper resolves
them to model-internal indices.

| Agent slot | Speaker label | Model index |
| --- | --- | ---: |
| Codex | `3922` | 0 |
| Claude | `8699` | 1 |
| AGY | `4535` | 2 |
| Copilot | `6701` | 3 |

These are evaluation choices, not proposed final defaults. The listening gate
may replace any speaker while retaining the same shared model.

## Default ONNX CPU arena

- Shared worker ready: 736 ms.
- RSS after loading the model: 150 MiB.
- Peak and final RSS after the long case: 427 MiB.

| Case | First audio | Total synthesis | CPU time | Audio duration | Real-time factor |
| --- | ---: | ---: | ---: | ---: | ---: |
| Short | 87 ms | 218 ms | 1.60 s | 3.9 s | 0.056 |
| Conversational | 112 ms | 1,147 ms | 8.50 s | 28.7 s | 0.040 |
| Technical prose | 51 ms | 437 ms | 3.37 s | 10.1 s | 0.043 |
| Switch: Codex | 166 ms | 167 ms | 1.28 s | 3.8 s | 0.044 |
| Switch: Claude | 136 ms | 137 ms | 1.07 s | 3.2 s | 0.042 |
| Switch: AGY | 140 ms | 141 ms | 1.11 s | 3.5 s | 0.041 |
| Switch: Copilot | 162 ms | 163 ms | 1.27 s | 4.1 s | 0.040 |
| Long throughput | 199 ms | 7,654 ms | 56.46 s | 194.4 s | 0.039 |

The long case consumed about 0.29 CPU-seconds per second of generated speech.
ONNX used roughly seven logical CPUs while synthesizing, but completed the work
about 25 times faster than playback.

## CPU arena disabled

The adapter constructed the ONNX session with `enable_cpu_mem_arena=False`.

- Shared worker ready: 726 ms.
- RSS after loading the model: 139 MiB.
- Transient peak RSS: 425 MiB.
- Final RSS after the long case: 252 MiB.
- Long-case synthesis: 9,722 ms, real-time factor 0.050.

Disabling the arena did not lower the transient memory required by inference,
but it reduced memory retained after the long case by about 41%. Long synthesis
was about 27% slower and still comfortably ahead of playback. This should be a
configuration choice rather than a universal default.

The arena comparison is single-variable. `PiperVoice.load()` creates a default
ONNX `SessionOptions` with `CPUExecutionProvider`; the comparison branch uses
the same provider and default options and changes only
`enable_cpu_mem_arena=False`.

## Segmentation equivalence

Piper's `synthesize` method already phonemizes the input into sentences and
performs one ONNX inference per sentence. For every fixed corpus case, its
internal sentence count and phoneme batches were identical to the planned
MoHuddle `(?<=[.!?])\\s+` segmentation. Piper inserts no extra silence between
the yielded PCM chunks, so the recorded listening files represent the planned
continuous-player boundary behavior for this corpus.

## Comparison with the initial four-model Piper run

| Measurement | Four single-speaker models | One LibriTTS-R model |
| --- | ---: | ---: |
| Model files | 242 MiB | 75 MiB |
| Worker ready | 2,407 ms | 736 ms |
| RSS after model load | 415 MiB | 150 MiB |
| Final RSS after long case | 1,013 MiB | 427 MiB |
| Warm first PCM range | 47–238 ms | 51–199 ms |

The shared model removes the earlier four-model memory concern without
sacrificing latency or per-agent voice stability. Human listening, real-device
time-to-first-sound, cancellation, and attribution/packaging decisions remain
open.

## Listening artifacts

The corrected WAV comparison is under:

```text
/tmp/mohuddle-tts-listening/piper-libritts/
```

The older `/tmp/mohuddle-tts-listening/piper/` directory is retained only as a
private voice-quality reference and must not be used to choose bundled defaults.

The user found this corrected set acceptable but preferred Kokoro in the first
listening comparison.
