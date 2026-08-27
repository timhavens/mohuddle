export const TRANSCRIPT_BOTTOM_THRESHOLD = 150;

export function normalizeExcerpt(value) {
  return String(value || "").replace(/\s+/gu, " ").trim();
}

export function isTranscriptAtBottom(metrics, threshold = TRANSCRIPT_BOTTOM_THRESHOLD) {
  const scrollHeight = Math.max(0, Number(metrics?.scrollHeight) || 0);
  const scrollY = Math.max(0, Number(metrics?.scrollY) || 0);
  const innerHeight = Math.max(0, Number(metrics?.innerHeight) || 0);
  return scrollHeight - scrollY - innerHeight <= threshold;
}

export function transcriptRenderState(metrics, unseen = 0, additions = 1) {
  const follow = isTranscriptAtBottom(metrics);
  return {
    follow,
    preserveScroll: !follow,
    unseen: follow ? 0 : Math.max(0, Number(unseen) || 0) + Math.max(0, Number(additions) || 0),
  };
}

function visiblePhoneMessage(message) {
  return Boolean(message && Number.isFinite(Number(message.sequence)));
}

function completedChatAnswer(message) {
  const author = String(message?.author || "").toLowerCase();
  return Boolean(message?.conversation_id)
    && String(message?.kind || "").toLowerCase() === "message"
    && author !== ""
    && author !== "user"
    && author !== "system";
}

// Returns the durable source question only when another visible transcript
// message separates it from its completed Chat answer.
export function replySourceFor(messages, answerIndex) {
  const answer = messages?.[answerIndex];
  if (!completedChatAnswer(answer)) {
    return null;
  }

  const source = messages
    .filter((message) => message?.author === "user" && message?.conversation_id === answer.conversation_id)
    .filter((message) => Number(message.sequence) < Number(answer.sequence))
    .sort((left, right) => Number(right.sequence) - Number(left.sequence))[0] || null;
  if (!source || !normalizeExcerpt(source.text)) {
    return null;
  }

  let previous = null;
  for (let index = 0; index < answerIndex; index += 1) {
    if (visiblePhoneMessage(messages[index])) {
      previous = messages[index];
    }
  }
  return Number(previous?.sequence) === Number(source.sequence) ? null : source;
}
