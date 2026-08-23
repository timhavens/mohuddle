import json
import sys

import numpy as np
from kokoro_onnx import Kokoro


def send_header(value):
    sys.stdout.buffer.write(json.dumps(value, separators=(",", ":")).encode("utf-8") + b"\n")
    sys.stdout.buffer.flush()


def main():
    if len(sys.argv) != 3:
        raise SystemExit("usage: kokoro-worker MODEL VOICES")

    engine = Kokoro(sys.argv[1], sys.argv[2])
    send_header({"type": "ready", "voices": engine.get_voices()})

    for raw_line in sys.stdin.buffer:
        try:
            request = json.loads(raw_line)
            if request.get("op") != "synthesize":
                raise ValueError("unsupported operation")
            audio, sample_rate = engine.create(
                str(request["text"]),
                voice=str(request["voice"]),
                speed=float(request.get("speed", 1.0)),
                lang=str(request.get("language", "en-us")),
            )
            payload = np.asarray(audio, dtype="<f4").reshape(-1).tobytes()
            send_header({"type": "audio", "bytes": len(payload), "sample_rate": int(sample_rate)})
            sys.stdout.buffer.write(payload)
            sys.stdout.buffer.flush()
        except Exception as error:
            send_header({"type": "error", "error": str(error)})


if __name__ == "__main__":
    main()
