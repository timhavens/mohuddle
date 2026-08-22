# TODO

## Completed

### Terminal interface

- [x] Improve the MoHuddle terminal UI with Codex-style interaction conveniences.
  - [x] Add persistent per-room submitted-message history with Up/Down and Ctrl+P/Ctrl+N recall.
  - [x] Add keyboard and mouse conversation scrollback without losing the draft or composer focus.
  - [x] Add a runtime mouse-capture toggle so normal terminal text selection remains available without permanently giving up mouse scrolling.
  - [x] Preserve and restore an unfinished draft, pasted blocks, and images while browsing history.
  - [x] Add a compact context footer showing both core agents' model, effort, access, and workspace, with the current target highlighted.
  - [x] Add compact pasted-content/image items, slash-command discovery, shortcut hints, and new-message navigation cues.
  - [x] Pass image paths pasted or dragged into the composer through unchanged as message text, allowing capable agents to inspect files they can access.

## Future considerations

### Voice participants

- [ ] Consider allowing AGY and Copilot to receive explicitly selected files as read-only context while preserving their voice-only, non-mutating role.
  - Prefer per-message file delivery rather than filesystem or directory grants.
  - Copilot can currently receive clipboard images through its SDK attachment mechanism, but it cannot open a path supplied as message text.
  - AGY currently cannot inspect image attachments or files referenced by path.
