import test from "node:test";
import assert from "node:assert/strict";

import {
  isTranscriptAtBottom,
  normalizeExcerpt,
  replySourceFor,
  transcriptRenderState,
} from "./static/timeline.mjs";

test("phone transcript follows new content only from the bottom", () => {
  const bottom = { scrollHeight: 1000, scrollY: 400, innerHeight: 600 };
  assert.equal(isTranscriptAtBottom(bottom), true);
  assert.deepEqual(transcriptRenderState(bottom, 3, 1), {
    follow: true,
    preserveScroll: false,
    unseen: 0,
  });

  const nearBottom = { scrollHeight: 1000, scrollY: 300, innerHeight: 600 };
  assert.equal(isTranscriptAtBottom(nearBottom), true);

  const intentionallyScrolled = { scrollHeight: 1000, scrollY: 200, innerHeight: 600 };
  assert.equal(isTranscriptAtBottom(intentionallyScrolled), false);
  assert.deepEqual(transcriptRenderState(intentionallyScrolled, 2, 3), {
    follow: false,
    preserveScroll: true,
    unseen: 5,
  });
});

test("phone reply excerpts appear only for separated completed Chat answers", () => {
  const question = { id: "q", sequence: 1, author: "user", kind: "message", conversation_id: "chat", text: "  What\nchanged? " };
  const answer = { id: "a", sequence: 3, author: "codex", kind: "message", conversation_id: "chat", text: "Answer" };
  assert.equal(replySourceFor([question, answer], 1), null);
  assert.equal(replySourceFor([question, { id: "other", sequence: 2, author: "claude", kind: "message" }, answer], 2), question);
  assert.equal(normalizeExcerpt(question.text), "What changed?");
});

test("phone reply excerpts retain sources across concurrent Chat jobs", () => {
  const first = { id: "q1", sequence: 1, author: "user", kind: "message", conversation_id: "chat-1", text: "first question" };
  const second = { id: "q2", sequence: 2, author: "user", kind: "message", conversation_id: "chat-2", text: "second question" };
  const other = { id: "other", sequence: 3, author: "claude", kind: "message", text: "workflow update" };
  const secondAnswer = { id: "a2", sequence: 4, author: "agy", kind: "message", conversation_id: "chat-2", text: "second answer" };
  const firstAnswer = { id: "a1", sequence: 5, author: "codex", kind: "message", conversation_id: "chat-1", text: "first answer" };
  const messages = [first, second, other, secondAnswer, firstAnswer];
  assert.equal(replySourceFor(messages, 3), second);
  assert.equal(replySourceFor(messages, 4), first);
});

test("phone reply excerpts use the latest follow-up prompt", () => {
  const question = { id: "q1", sequence: 1, author: "user", kind: "message", conversation_id: "chat", text: "What changed?" };
  const firstAnswer = { id: "a1", sequence: 2, author: "codex", kind: "message", conversation_id: "chat", text: "Initial answer" };
  const followUp = { id: "q2", sequence: 4, author: "user", kind: "message", conversation_id: "chat", text: "  Why\n did it change? " };
  const adjacentAnswer = { id: "a2", sequence: 5, author: "claude", kind: "message", conversation_id: "chat", text: "Follow-up answer" };

  assert.equal(replySourceFor([question, firstAnswer, followUp, adjacentAnswer], 3), null);

  const other = { id: "other", sequence: 5, author: "agy", kind: "message", text: "another visible answer" };
  const separatedAnswer = { ...adjacentAnswer, sequence: 6 };
  assert.equal(replySourceFor([question, firstAnswer, followUp, other, separatedAnswer], 4), followUp);
  assert.equal(normalizeExcerpt(followUp.text), "Why did it change?");
});

test("phone answer renders normally when recovery omitted its source", () => {
  const answer = { id: "a", sequence: 5, author: "codex", kind: "message", conversation_id: "chat", text: "answer" };
  const messages = [{ id: "other", sequence: 4, author: "claude", kind: "message", text: "visible" }, answer];
  assert.equal(replySourceFor(messages, 1), null);
});
