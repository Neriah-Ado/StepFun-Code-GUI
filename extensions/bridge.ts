/**
 * Process bridge: spawns the Go gateway and feeds it newline-delimited JSON.
 *
 * Design notes:
 * - The extension must never block or throw into the host's agent loop. Every
 *   failure degrades to a one-shot warning; Step Code keeps running.
 * - The gateway is launched lazily on the first `start()` call, then kept alive
 *   for the whole session so the browser panel survives reconnects.
 * - A crash is not fatal: the bridge re-spawns on the next message.
 */
import { spawn, type ChildProcess } from "node:child_process";
import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import type { BridgeAction, BridgeMessage } from "./types.ts";

const HERE = dirname(fileURLToPath(import.meta.url));
const PACKAGE_ROOT = resolve(HERE, "..");

const BINARY_NAME =
	process.platform === "win32" ? "step-orchestra-gateway.exe" : "step-orchestra-gateway";

/** Resolution order: explicit override, shipped binary, dev build output. */
const BINARY_CANDIDATES: string[] = [
	process.env.STEP_ORCHESTRA_BIN ?? "",
	resolve(PACKAGE_ROOT, "bin", BINARY_NAME),
	resolve(PACKAGE_ROOT, "gateway", BINARY_NAME),
].filter((candidate) => candidate.length > 0);

/** The gateway prints exactly one such line on startup. */
const PANEL_URL_PATTERN = /https?:\/\/127\.0\.0\.1:\d+\/\?t=[A-Za-z0-9_-]+/;

export class Bridge {
	private child: ChildProcess | null = null;
	private pending: string[] = [];
	private stdoutTail = "";
	private pendingStdout = "";
	private actionHandler: ((action: BridgeAction) => void) | null = null;
	private panelUrl: string | null = null;
	private failureReason: string | null = null;
	private warned = false;
	private disposed = false;

	/** Launch the gateway if it is not already running. Safe to call repeatedly. */
	start(): void {
		if (this.disposed || this.child) return;

		const binary = BINARY_CANDIDATES.find((candidate) => existsSync(candidate));
		if (!binary) {
			this.failureReason = `gateway binary missing (looked in ${BINARY_CANDIDATES.join(", ")})`;
			return;
		}

		let child: ChildProcess;
		try {
			child = spawn(binary, [], {
				stdio: ["pipe", "pipe", "pipe"],
				env: {
					...process.env,
					STEP_ORCHESTRA_WEB: process.env.STEP_ORCHESTRA_WEB ?? resolve(PACKAGE_ROOT, "web"),
					// Where the gateway keeps credential profiles. Placed beside the
					// host's own agent state so it inherits the same directory.
					STEP_ORCHESTRA_HOME:
						process.env.STEP_ORCHESTRA_HOME ??
						join(homedir(), ".stepcode", "agent", "step-orchestra"),
				},
				windowsHide: true,
			});
		} catch (error) {
			this.failureReason = `gateway spawn threw: ${String(error)}`;
			return;
		}

		child.on("error", (error) => {
			this.failureReason = `gateway error: ${error.message}`;
			this.child = null;
		});
		child.on("exit", (code) => {
			this.child = null;
			if (!this.disposed && code !== 0 && code !== null) {
				this.failureReason = `gateway exited with code ${code}`;
			}
		});

		child.stdout?.setEncoding("utf8");
		child.stdout?.on("data", (chunk: string) => this.absorbStdout(chunk));
		child.stderr?.setEncoding("utf8");
		child.stderr?.on("data", (chunk: string) => process.stderr.write(chunk));

		this.child = child;
		this.failureReason = null;

		for (const line of this.pending) child.stdin?.write(`${line}\n`);
		this.pending.length = 0;
	}

	/** Queue one message. Never throws. */
	send(message: BridgeMessage): void {
		if (this.disposed) return;

		const line = JSON.stringify(message);

		if (!this.child) {
			this.start();
			if (!this.child) {
				this.warnOnce();
				return;
			}
		}

		const stdin = this.child.stdin;
		if (!stdin || stdin.destroyed || !stdin.writable) {
			this.child = null;
			this.pending.push(line);
			return;
		}

		// `write` returning false means the kernel buffer is full, NOT that the
		// write failed — Node buffers the remainder and flushes it as the pipe
		// drains. Treating it as an error would drop the message for no reason.
		stdin.write(`${line}\n`);
	}

	/** Register the reverse-channel handler for gateway-originated actions. */
	onAction(handler: (action: BridgeAction) => void): void {
		this.actionHandler = handler;
	}

	/** Panel URL, available once the gateway has printed it. */
	getPanelUrl(): string | null {
		return this.panelUrl;
	}

	/** Human-readable reason the bridge is inert, if any. */
	getFailureReason(): string | null {
		return this.failureReason;
	}

	/** Tell the gateway to exit, then drop the handle. */
	dispose(): void {
		this.disposed = true;
		const child = this.child;
		this.child = null;
		if (!child) return;
		try {
			child.stdin?.write(`${JSON.stringify({ kind: "shutdown", ts: Date.now() })}\n`);
			child.stdin?.end();
		} catch {
			// stdin already gone; the kill below is enough
		}
		const timer = setTimeout(() => {
			try {
				child.kill();
			} catch {
				// already reaped
			}
		}, 1500);
		timer.unref?.();
	}

	/**
	 * Consume gateway stdout. Action lines go to the reverse-channel handler;
	 * everything else is mirrored to the host log. The panel URL is scraped on
	 * the way through so `/orchestra` can print it.
	 */
	private absorbStdout(chunk: string): void {
		this.stdoutTail = (this.stdoutTail + chunk).slice(-2048);
		if (!this.panelUrl) {
			const match = this.stdoutTail.match(PANEL_URL_PATTERN);
			if (match) this.panelUrl = match[0];
		}

		this.pendingStdout += chunk;
		const lines = this.pendingStdout.split("\n");
		// Keep the trailing partial line for the next chunk.
		this.pendingStdout = lines.pop() ?? "";

		for (const line of lines) {
			const trimmed = line.trim();
			if (!trimmed) continue;

			if (trimmed.charCodeAt(0) === 0x7b && this.actionHandler) {
				try {
					const parsed = JSON.parse(trimmed) as Partial<BridgeAction>;
					if (parsed && parsed.type === "action") {
						this.actionHandler(parsed as BridgeAction);
						continue;
					}
				} catch {
					// not JSON — fall through and mirror it as a log line
				}
			}

			process.stderr.write(`${trimmed}\n`);
		}
	}

	private warnOnce(): void {
		if (this.warned) return;
		this.warned = true;
		const reason = this.failureReason ?? "unknown reason";
		process.stderr.write(
			`[step-orchestra] live panel unavailable: ${reason}\n` +
				`[step-orchestra] build it with: cd gateway && go build -o ../bin/${BINARY_NAME} .\n`,
		);
	}
}
