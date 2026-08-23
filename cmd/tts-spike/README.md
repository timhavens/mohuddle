# TTS provider spike harness

This command benchmarks an external TTS adapter without connecting it to live
MoHuddle messages. It launches the adapter directly (never through a shell),
sends corpus text on stdin unless an argument contains `{text}`, and measures
time to first output byte, total time, CPU time, peak Linux RSS, and audio bytes.

The adapter must write only audio bytes to stdout and diagnostics to stderr.
Use `{voice}` in an argument to insert the voice mapped for each corpus slot.

Example with a hypothetical raw-PCM adapter:

```bash
go run ./cmd/tts-spike \
  --provider kokoro-local \
  --binary /absolute/path/to/adapter \
  --arg --voice \
  --arg '{voice}' \
  --voice codex=voice-a \
  --voice claude=voice-b \
  --voice agy=voice-c \
  --voice copilot=voice-d \
  --runs 3 \
  --extension pcm \
  --output-dir /tmp/mohuddle-tts-audio \
  --report /tmp/mohuddle-tts-report.json
```

`first_audio_byte_ms` is a transport measurement, not proof of audible speech.
The provider spike must also record a player-start proxy and perform the manual
WSL listening checks described in `TODO.md`.

The harness currently starts one process per case. This deliberately establishes
a cold CLI baseline. A provider-specific persistent-worker adapter must add warm
measurements before the decision gate; do not compare this cold result to a warm
server result without labeling the difference.

## Piper warm-worker comparison

`piper_warm.py` loads every mapped Piper voice once, then runs the same corpus in
one process. It measures model-load time, worker-ready time, first synthesized
audio, total synthesis time, real-time factor, and resident memory. It writes raw
signed 16-bit mono PCM at the sample rate declared by each voice model.

Run it with an isolated Piper installation on `PYTHONPATH`:

```bash
PYTHONPATH=/tmp/piper/site python3 cmd/tts-spike/piper_warm.py \
  --corpus cmd/tts-spike/testdata/corpus.json \
  --voice codex=/tmp/piper/voices/en_US-ryan-medium.onnx \
  --voice claude=/tmp/piper/voices/en_US-amy-medium.onnx \
  --voice agy=/tmp/piper/voices/en_US-hfc_male-medium.onnx \
  --voice copilot=/tmp/piper/voices/en_US-hfc_female-medium.onnx \
  --runs 3 \
  --output-dir /tmp/piper/warm-audio \
  --report /tmp/piper/warm-report.json
```

This script is an evaluation adapter, not the proposed production worker
protocol. Its four-model memory result is specifically intended to decide
whether keeping one Piper model warm per agent is acceptable.

For a multi-speaker model, load one ONNX session and map each agent to a speaker
label from the model configuration:

```bash
PYTHONPATH=/tmp/piper/site python3 cmd/tts-spike/piper_warm.py \
  --corpus cmd/tts-spike/testdata/corpus.json \
  --shared-model /tmp/piper/voices/en_US-libritts_r-medium.onnx \
  --speaker codex=3922 \
  --speaker claude=8699 \
  --speaker agy=4535 \
  --speaker copilot=6701 \
  --output-dir /tmp/piper/shared-audio \
  --report /tmp/piper/shared-report.json
```

Add `--disable-cpu-arena` to either warm adapter to construct its ONNX session
with the CPU memory arena disabled. This is a measurement control, not assumed
to improve memory: it reduced retained Piper RSS but made Kokoro's peak and
retained RSS worse on the recorded WSL host.

## Kokoro ONNX warm-worker comparison

`kokoro_warm.py` loads one Kokoro ONNX model and its shared voice bank, then
calls synchronous `Kokoro.create()` once per normalized sentence with distinct
voice names. It writes raw little-endian float32 mono samples and measures worker
load, first synthesized audio, per-segment and total synthesis, real-time factor,
and memory.

```bash
/tmp/kokoro/venv/bin/python cmd/tts-spike/kokoro_warm.py \
  --model /tmp/kokoro/models/kokoro-v1.0.onnx \
  --voices-file /tmp/kokoro/models/voices-v1.0.bin \
  --corpus cmd/tts-spike/testdata/corpus.json \
  --voice codex=am_adam \
  --voice claude=af_sarah \
  --voice agy=am_michael \
  --voice copilot=af_nova \
  --runs 3 \
  --output-dir /tmp/kokoro/audio \
  --report /tmp/kokoro/report.json
```

The synchronous path deliberately avoids `Kokoro.create_stream()`, whose
untracked background task does not propagate consumer cancellation. Initial
soft cancellation stops playback immediately, discards the current synchronous
result when it returns, and issues no later segment. Production segmentation
should keep calls reasonably sized, but no hard synthesis-time ceiling is
claimed initially.
