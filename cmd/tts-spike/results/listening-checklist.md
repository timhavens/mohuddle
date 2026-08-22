# Local TTS listening review

The automated spike measures synthesis performance but cannot decide whether a
voice is pleasant, intelligible, or suitable for a particular agent. Review the
generated WAV files with real WSL audio before selecting a provider.

The current comparison set is generated outside the repository at:

```text
/tmp/mohuddle-tts-listening/
    piper-libritts/
    kokoro/
```

For each provider, listen to `short`, `conversational`, `technical`, and
`voice-switch`. Sample the beginning, middle, and end of `long-throughput`;
listening to the complete response is useful for detecting fatigue and
discontinuities.

Suggested playback:

```bash
mpv /tmp/mohuddle-tts-listening/piper-libritts/short.wav
mpv /tmp/mohuddle-tts-listening/kokoro/short.wav
```

Score each provider from 1 (poor) to 5 (excellent):

| Criterion | Piper | Kokoro | Notes |
| --- | ---: | ---: | --- |
| Naturalness | | | |
| General intelligibility | | | |
| Technical terms and commands | | | |
| Four voices are easy to distinguish | | | |
| Pacing and pauses | | | |
| No clicks or gaps at sentence boundaries | | | |
| Comfortable during a long response | | | |

Also note any consistently mispronounced agent names, Markdown artifacts, or
voice/model combinations that should be excluded. These samples are prerecorded,
so they do not validate audible time-to-first-sound; that remains a separate live
player test after a provider passes this listening review.

The older `piper/` sample set uses voice models with noncommercial or unresolved
terms. It is retained for private comparison only; use `piper-libritts/` for the
provider-selection gate because its shared model is CC BY 4.0.

## Recorded result

On 2026-08-22 the user found both corrected provider sets acceptable and
preferred Kokoro. This passes the initial human quality gate for both providers;
it does not replace the remaining live latency, cancellation, contention, and
runtime audit gates.
