// 渲染 Session 的辅助：正在生成的草稿与 ask_user 提问（docs/design/m3-duplex-channel.md §4、§7）。

import type { Event, ToolCall } from "./gen/yanshi/v1/event_pb.js";
import type { LiveDelta } from "./gen/yanshi/v1/node_pb.js";

/**
 * Draft 维护一个 Session 正在生成、尚未提交的助手消息，规则见设计 §4：
 *   - snapshot 增量替换草稿（生成中途订阅时先收到它）；普通增量追加；
 *   - end 增量，或 seq 大于草稿 afterSeq 的 AssistantMessage 到达时，丢弃草稿：以提交的内容为准。
 *
 * 把订阅收到的增量与事件依次交给 onDelta 与 onEvent，渲染 text 即可。
 */
export class Draft {
  /** 草稿全文；没有正在生成的消息时为空。 */
  text = "";
  /** 草稿所属的 Run；没有正在生成的消息时为空。 */
  runId = "";
  #attempt = 0;
  #afterSeq = -1n;
  // 已提交的最新 AssistantMessage 的 seq：after_seq 小于它的增量属于已提交的生成，是迟到的残留。
  #committed = 0n;

  get active(): boolean {
    return this.runId !== "";
  }

  /** 应用一条增量；返回草稿是否变化。 */
  onDelta(d: LiveDelta): boolean {
    if (d.afterSeq < this.#committed) return false;
    const same = d.runId === this.runId && d.attempt === this.#attempt && d.afterSeq === this.#afterSeq;
    if (d.end) {
      if (!same) return false;
      this.#clear();
      return true;
    }
    if (d.snapshot || !same) {
      this.runId = d.runId;
      this.#attempt = d.attempt;
      this.#afterSeq = d.afterSeq;
      this.text = d.text;
      return true;
    }
    this.text += d.text;
    return d.text !== "";
  }

  /** 应用一条已提交的事件；返回草稿是否变化。 */
  onEvent(e: Event): boolean {
    if (e.payload.case !== "assistantMessage") return false;
    if (e.seq > this.#committed) this.#committed = e.seq;
    if (!this.active || e.seq <= this.#afterSeq) return false;
    this.#clear();
    return true;
  }

  #clear(): void {
    this.text = "";
    this.runId = "";
    this.#attempt = 0;
    this.#afterSeq = -1n;
  }
}

/** ask_user 的能力名。 */
export const ASK_USER = "ask_user";

export interface QuestionOption {
  id: string;
  label: string;
  description?: string;
}

export interface QuestionField {
  name: string;
  label: string;
  /** "text"（默认）、"number"、"date"（YYYY-MM-DD）或 "time"（HH:MM）。 */
  type?: "text" | "number" | "date" | "time";
  required?: boolean;
}

/** Agent 经 ask_user 提出的问题（ADR-0025）。 */
export interface Question {
  callId: string;
  question: string;
  options: QuestionOption[];
  multiple: boolean;
  fields: QuestionField[];
}

/**
 * 从 AssistantMessage 的工具调用中读出 ask_user 提问；不是提问或参数无法解析时返回 undefined。
 * 参数不合法的提问不会到达用户（Worker 直接把错误交还模型），所以这里不再校验。
 */
export function questionOf(call: ToolCall): Question | undefined {
  if (call.capability !== ASK_USER) return undefined;
  try {
    const a = JSON.parse(call.argumentsJson) as Partial<Omit<Question, "callId">>;
    return {
      callId: call.callId,
      question: a.question ?? "",
      options: a.options ?? [],
      multiple: a.multiple ?? false,
      fields: a.fields ?? [],
    };
  } catch {
    return undefined;
  }
}

/**
 * PendingQuestions 跟踪一个 Session 中等待回答的提问：提问随 AssistantMessage 出现，
 * 在该调用出现 ToolResult（任一设备回答、超时）或 Run 结束时收起。
 */
export class PendingQuestions {
  readonly #open = new Map<string, Question & { runId: string }>();

  get questions(): Question[] {
    return [...this.#open.values()];
  }

  /** 应用一条已提交的事件；返回列表是否变化。 */
  onEvent(e: Event): boolean {
    const p = e.payload;
    switch (p.case) {
      case "assistantMessage": {
        let changed = false;
        for (const c of p.value.toolCalls) {
          const q = questionOf(c);
          if (q) {
            this.#open.set(q.callId, { ...q, runId: p.value.runId });
            changed = true;
          }
        }
        return changed;
      }
      case "toolResult":
        return this.#open.delete(p.value.callId);
      case "runCompleted":
      case "runFailed":
      case "runInterrupted": {
        let changed = false;
        for (const [id, q] of this.#open) {
          if (q.runId === p.value.runId) changed = this.#open.delete(id) || changed;
        }
        return changed;
      }
      default:
        return false;
    }
  }
}
