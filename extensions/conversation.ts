/**
 * Conversation mirror.
 *
 * Keeps the browser chat view aligned with the host session: replays history
 * when a session starts, streams assistant text as it is produced, and forwards
 * send requests arriving from the browser back into the extension API.
 *
 * All host payload access is defensive — a renamed field degrades to a missing
 * message rather than a broken panel.
 */
import { firstString } from "./redact.ts";
import type { BridgeAction, BridgeMessage, ConversationMessage } from "./types.ts";

/** Bounds so a long session cannot grow the panel without limit. */
const MAX_MESSAGES = 400;
const MAX_TEXT = 20000;
const MAX_ENTRIES_SCANNED = 4000;

/**
 * Coalescing window for streamed deltas. A delta carries only the increment,
 * but the browser re-renders the whole bubble, so forwarding one frame per token
 * is O(n²) in reply length. 80ms keeps the typing feel while cutting frames by
 * roughly 5x; `finalizeMessage` still sends the authoritative full text.
 */
const DELTA_FLUSH_MS = 80;

/** Fallback id for a streaming assistant reply with no message id. */
const STREAM_ID = "assistant-stream";

export interface ConversationOptions {
	emit: (message: BridgeMessage) => void;
	sendUserMessage: (text: string, deliverAs?: "steer" | "followUp") => void;
	/** Whether the agent is idle; gates a plain send, which throws when busy. */
	isIdle: () => boolean;
}

export interface Conversation {
	hydrate(entries: readonly unknown[]): void;
	recordMessage(event: unknown): void;
	applyDelta(event: unknown): void;
	finalizeMessage(event: unknown): void;
	handleAction(action: BridgeAction): void;
}

export function createConversation(options: ConversationOptions): Conversation {
	const { emit, sendUserMessage, isIdle } = options;

	const messages: ConversationMessage[] = [];
	const index = new Map<string, ConversationMessage>();

	function publish(message: ConversationMessage): void {
		emit({ kind: "conversation", ts: Date.now(), message });
	}

	function upsert(message: ConversationMessage): ConversationMessage {
		const existing = index.get(message.id);
		if (existing) {
			Object.assign(existing, message);
			publish(existing);
			return existing;
		}

		messages.push(message);
		index.set(message.id, message);

		if (messages.length > MAX_MESSAGES) {
			for (const dropped of messages.splice(0, messages.length - MAX_MESSAGES)) {
				index.delete(dropped.id);
			}
		}

		publish(message);
		return message;
	}

	/** Rebuild from host session entries so a fresh tab renders past turns. */
	function hydrate(entries: readonly unknown[]): void {
		// A replay supersedes any in-flight stream frame.
		cancelFlush();

		const replayed: ConversationMessage[] = [];
		const slice = entries.slice(-MAX_ENTRIES_SCANNED);

		slice.forEach((entry, position) => {
			const record = asRecord(entry);
			if (!record || record.type !== "message") return;

			const message = asRecord(record.message);
			if (!message) return;

			const role = message.role;
			if (role !== "user" && role !== "assistant") return;

			const text = clip(textOf(message.content));
			if (!text) return;

			replayed.push({
				id: firstString(record.id, message.id) || `entry-${position}`,
				role,
				text,
				at: numberOr(record.timestamp, message.timestamp) ?? 0,
			});
		});

		messages.length = 0;
		index.clear();
		for (const message of replayed.slice(-MAX_MESSAGES)) {
			messages.push(message);
			index.set(message.id, message);
		}

		emit({ kind: "conversation_history", ts: Date.now(), messages: messages.slice() });
	}

	function roleOf(message: Record<string, any>): "user" | "assistant" | null {
		const role = message.role;
		return role === "user" || role === "assistant" ? role : null;
	}

	function recordMessage(event: unknown): void {
		const record = asRecord(event);
		const message = asRecord(record?.message);
		if (!message) return;

		const role = roleOf(message);
		if (!role) return;

		const id =
			firstString(message.id, record?.messageId) ||
			(role === "assistant" ? STREAM_ID : `msg-${Date.now()}-${messages.length}`);

		upsert({
			id,
			role,
			text: clip(textOf(message.content)),
			at: Date.now(),
			streaming: role === "assistant",
		});
	}

	/** Pending coalesced frame, see DELTA_FLUSH_MS. */
	let flushTimer: ReturnType<typeof setTimeout> | null = null;
	let flushTarget: ConversationMessage | null = null;

	function cancelFlush(): void {
		if (!flushTimer) return;
		clearTimeout(flushTimer);
		flushTimer = null;
		flushTarget = null;
	}

	function scheduleFlush(target: ConversationMessage): void {
		flushTarget = target;
		if (flushTimer) return;

		flushTimer = setTimeout(() => {
			flushTimer = null;
			const message = flushTarget;
			flushTarget = null;
			if (message) publish(message);
		}, DELTA_FLUSH_MS);

		// Never hold the host process open just to emit one frame.
		(flushTimer as unknown as { unref?: () => void }).unref?.();
	}

	function applyDelta(event: unknown): void {
		const record = asRecord(event);
		const streamEvent = asRecord(record?.assistantMessageEvent);
		if (!streamEvent || streamEvent.type !== "text_delta") return;

		const delta = typeof streamEvent.delta === "string" ? streamEvent.delta : "";
		if (!delta) return;

		const message = asRecord(record?.message);
		const id = firstString(message?.id, record?.messageId) || STREAM_ID;

		let target = index.get(id);
		if (!target) {
			// Insert without publishing: the scheduled flush carries the first text,
			// so a new bubble is never announced empty and then immediately again.
			target = { id, role: "assistant", text: "", at: Date.now(), streaming: true };
			messages.push(target);
			index.set(id, target);
		}

		target.text = clip(target.text + delta);
		target.streaming = true;
		scheduleFlush(target);
	}

	function finalizeMessage(event: unknown): void {
		// This publish is authoritative; drop any coalesced frame behind it.
		cancelFlush();

		const record = asRecord(event);
		const message = asRecord(record?.message);
		if (!message) return;

		const role = roleOf(message);
		if (!role) return;

		const id =
			firstString(message.id, record?.messageId) ||
			(role === "assistant" ? STREAM_ID : `msg-${Date.now()}`);

		const full = clip(textOf(message.content));
		const existing = index.get(id);

		if (existing) {
			// The finalized text is authoritative; the delta buffer may have drifted.
			if (full) existing.text = full;
			existing.streaming = false;
			publish(existing);
			return;
		}

		upsert({ id, role, text: full, at: Date.now(), streaming: false });
	}

	function handleAction(action: BridgeAction): void {
		if (action.action !== "send") return;

		const text = typeof action.text === "string" ? action.text.trim() : "";
		if (!text) {
			emit({
				kind: "action_ack",
				ts: Date.now(),
				requestId: action.requestId,
				ok: false,
				error: "empty message",
			});
			return;
		}

		try {
			// A plain send throws while the agent is mid-run, so queue instead.
			if (isIdle()) {
				sendUserMessage(text);
			} else {
				sendUserMessage(text, action.deliverAs ?? "followUp");
			}
			emit({ kind: "action_ack", ts: Date.now(), requestId: action.requestId, ok: true });
		} catch (error) {
			emit({
				kind: "action_ack",
				ts: Date.now(),
				requestId: action.requestId,
				ok: false,
				error: error instanceof Error ? error.message : String(error),
			});
		}
	}

	return { hydrate, recordMessage, applyDelta, finalizeMessage, handleAction };
}

// ------------------------------------------------------------------ helpers

function asRecord(value: unknown): Record<string, any> | null {
	if (value === null || typeof value !== "object") return null;
	return value as Record<string, any>;
}

/** Flatten a message content block list into plain text. */
function textOf(content: unknown): string {
	if (typeof content === "string") return content;
	if (!Array.isArray(content)) return "";

	const parts: string[] = [];
	for (const block of content) {
		const record = asRecord(block);
		if (!record) continue;
		if (record.type === "text" && typeof record.text === "string") parts.push(record.text);
	}
	return parts.join("");
}

function clip(text: string): string {
	return text.length > MAX_TEXT ? `${text.slice(0, MAX_TEXT)}…[truncated]` : text;
}

function numberOr(...candidates: unknown[]): number | undefined {
	for (const candidate of candidates) {
		if (typeof candidate === "number" && Number.isFinite(candidate)) return candidate;
	}
	return undefined;
}
