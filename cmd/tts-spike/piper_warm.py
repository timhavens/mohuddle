#!/usr/bin/env python3
"""Measure warm Piper synthesis with all configured voices loaded once."""

import argparse
import datetime
import importlib.metadata
import json
import os
import pathlib
import resource
import sys
import time

import onnxruntime

from piper import PiperConfig, PiperVoice, SynthesisConfig


def parse_voice(value):
    slot, separator, model = value.partition("=")
    slot = slot.strip()
    model = model.strip()
    if not separator or not slot or not model:
        raise argparse.ArgumentTypeError("voice must use SLOT=/absolute/model.onnx")
    return slot, model


def parse_speaker(value):
    slot, separator, speaker = value.partition("=")
    slot = slot.strip()
    speaker = speaker.strip()
    if not separator or not slot or not speaker:
        raise argparse.ArgumentTypeError("speaker must use SLOT=SPEAKER_NAME_OR_ID")
    return slot, speaker


def parse_args():
    parser = argparse.ArgumentParser()
    parser.add_argument("--corpus", required=True)
    parser.add_argument("--voice", action="append", type=parse_voice)
    parser.add_argument("--shared-model")
    parser.add_argument("--speaker", action="append", type=parse_speaker)
    parser.add_argument("--disable-cpu-arena", action="store_true")
    parser.add_argument("--runs", type=int, default=1)
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--report", required=True)
    args = parser.parse_args()
    if args.runs < 1:
        parser.error("--runs must be positive")
    if bool(args.voice) == bool(args.shared_model):
        parser.error("use either --voice mappings or --shared-model, but not both")
    if args.shared_model and not args.speaker:
        parser.error("--shared-model requires at least one --speaker mapping")
    if args.voice and args.speaker:
        parser.error("--speaker is only valid with --shared-model")
    return args


def read_rss_bytes():
    try:
        for line in pathlib.Path("/proc/self/status").read_text().splitlines():
            if line.startswith("VmRSS:"):
                return int(line.split()[1]) * 1024
    except (OSError, ValueError, IndexError):
        pass
    # Linux reports KiB; this spike currently targets Debian/WSL.
    return int(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss) * 1024


def expanded_text(item):
    repeat = max(1, int(item.get("repeat", 1)))
    return " ".join(item["text"].strip() for _ in range(repeat))


def safe_case_id(value):
    if not value or value in (".", "..") or "/" in value or "\\" in value:
        raise ValueError("invalid case id: {!r}".format(value))
    return value


def load_voice(model_path, disable_cpu_arena):
    if not disable_cpu_arena:
        return PiperVoice.load(str(model_path))

    config_path = pathlib.Path(str(model_path) + ".json")
    config = PiperConfig.from_dict(json.loads(config_path.read_text()))
    options = onnxruntime.SessionOptions()
    options.enable_cpu_mem_arena = False
    session = onnxruntime.InferenceSession(
        str(model_path), sess_options=options, providers=["CPUExecutionProvider"]
    )
    return PiperVoice(session=session, config=config)


def resolve_speaker(voice, selector):
    if selector in voice.config.speaker_id_map:
        return voice.config.speaker_id_map[selector]
    try:
        speaker_id = int(selector)
    except ValueError as error:
        raise ValueError("unknown speaker {!r}".format(selector)) from error
    if not 0 <= speaker_id < voice.config.num_speakers:
        raise ValueError("speaker ID {} is out of range".format(speaker_id))
    return speaker_id


def main():
    args = parse_args()
    corpus_path = pathlib.Path(args.corpus)
    suite = json.loads(corpus_path.read_text())
    if suite.get("version") != 1 or not suite.get("cases"):
        raise ValueError("corpus must use version 1 and contain cases")

    output_dir = pathlib.Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    voices = {}
    synthesis_configs = {}
    speaker_details = {}
    loads = []
    worker_started = time.perf_counter()
    if args.shared_model:
        model_path = pathlib.Path(args.shared_model).resolve()
        if not model_path.is_file():
            raise FileNotFoundError(model_path)
        started = time.perf_counter()
        voice = load_voice(model_path, args.disable_cpu_arena)
        loads.append(
            {
                "voice_slot": "shared",
                "model": str(model_path),
                "load_ms": round((time.perf_counter() - started) * 1000, 3),
                "rss_bytes": read_rss_bytes(),
                "sample_rate": voice.config.sample_rate,
            }
        )
        for slot, selector in dict(args.speaker).items():
            speaker_id = resolve_speaker(voice, selector)
            voices[slot] = voice
            synthesis_configs[slot] = SynthesisConfig(speaker_id=speaker_id)
            speaker_details[slot] = {
                "speaker": selector,
                "speaker_id": speaker_id,
            }
        process_mode = "one_process_shared_multi_speaker_model"
    else:
        for slot, model_value in dict(args.voice).items():
            model_path = pathlib.Path(model_value).resolve()
            if not model_path.is_file():
                raise FileNotFoundError(model_path)
            started = time.perf_counter()
            voice = load_voice(model_path, args.disable_cpu_arena)
            loads.append(
                {
                    "voice_slot": slot,
                    "model": str(model_path),
                    "load_ms": round((time.perf_counter() - started) * 1000, 3),
                    "rss_bytes": read_rss_bytes(),
                    "sample_rate": voice.config.sample_rate,
                }
            )
            voices[slot] = voice
            synthesis_configs[slot] = SynthesisConfig()
        process_mode = "one_process_all_voices_loaded"
    worker_ready_ms = round((time.perf_counter() - worker_started) * 1000, 3)

    results = []
    for item in suite["cases"]:
        case_id = safe_case_id(item.get("id"))
        slot = item.get("voice_slot", "")
        if slot not in voices:
            raise ValueError("case {!r} requires voice slot {!r}".format(case_id, slot))
        voice = voices[slot]
        synthesis_config = synthesis_configs[slot]
        text = expanded_text(item)
        for run in range(1, args.runs + 1):
            output_path = output_dir / "{}-run-{:02d}.pcm".format(case_id, run)
            started = time.perf_counter()
            cpu_started = time.process_time()
            first_audio_ms = None
            audio_bytes = 0
            sample_rate = voice.config.sample_rate
            with output_path.open("wb") as output_file:
                for chunk in voice.synthesize(text, synthesis_config):
                    if first_audio_ms is None:
                        first_audio_ms = round((time.perf_counter() - started) * 1000, 3)
                    output_file.write(chunk.audio_int16_bytes)
                    audio_bytes += len(chunk.audio_int16_bytes)
                    sample_rate = chunk.sample_rate
            total_ms = round((time.perf_counter() - started) * 1000, 3)
            duration_seconds = audio_bytes / float(sample_rate * 2)
            results.append(
                {
                    "case_id": case_id,
                    "voice_slot": slot,
                    "run": run,
                    "characters": len(text),
                    "first_audio_ms": first_audio_ms,
                    "total_ms": total_ms,
                    "cpu_ms": round((time.process_time() - cpu_started) * 1000, 3),
                    "rss_bytes": read_rss_bytes(),
                    "audio_bytes": audio_bytes,
                    "audio_duration_seconds": round(duration_seconds, 3),
                    "real_time_factor": round((total_ms / 1000) / duration_seconds, 5),
                    "sample_rate": sample_rate,
                    "output": str(output_path),
                    **speaker_details.get(slot, {}),
                }
            )

    report = {
        "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "provider": "piper-warm",
        "provider_version": importlib.metadata.version("piper-tts"),
        "python": sys.version,
        "process_mode": process_mode,
        "cpu_mem_arena_enabled": not args.disable_cpu_arena,
        "corpus": str(corpus_path),
        "worker_ready_ms": worker_ready_ms,
        "loads": loads,
        "peak_rss_bytes": int(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss) * 1024,
        "results": results,
    }
    report_path = pathlib.Path(args.report)
    report_path.parent.mkdir(parents=True, exist_ok=True)
    report_path.write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    main()
