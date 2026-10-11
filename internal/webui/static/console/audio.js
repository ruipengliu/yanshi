// 浏览器音频：麦克风采集（16 kHz PCM 16 位，20 ms 一帧）、语音消息的 WAV 编码、通话下行音频的播放与打断。
// 采集与播放的做法同通话页（static/call.js）：上行在 AudioWorklet 中重采样，下行按到达顺序排队播放。

const WORKLET = `
class Mic extends AudioWorkletProcessor {
  constructor() { super(); this.ratio = sampleRate / 16000; this.t = 0; this.prev = 0; this.out = new Int16Array(320); this.n = 0; this.peak = 0; }
  process(inputs) {
    const x = inputs[0][0];
    if (!x) return true;
    while (this.t < x.length - 1) {
      const i = Math.floor(this.t), f = this.t - i;
      const a = i < 0 ? this.prev : x[i], b = x[i + 1];
      const v = Math.max(-1, Math.min(1, a + (b - a) * f));
      this.peak = Math.max(this.peak, Math.abs(v));
      this.out[this.n++] = v * 0x7fff;
      if (this.n === 320) { this.port.postMessage({ pcm: this.out.slice(), peak: this.peak }); this.n = 0; this.peak = 0; }
      this.t += this.ratio;
    }
    this.t -= x.length;
    this.prev = x[x.length - 1];
    return true;
  }
}
registerProcessor("mic", Mic);`;

/** Mic 打开麦克风，每 20 ms 以 onFrame(pcm: Int16Array, peak: number) 交出一帧 16 kHz 音频。 */
export class Mic {
  async start(onFrame) {
    this.stream = await navigator.mediaDevices.getUserMedia({
      audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true, channelCount: 1 },
    });
    let ctx;
    let src;
    try {
      ctx = new AudioContext({ sampleRate: 16000 });
      src = ctx.createMediaStreamSource(this.stream);
    } catch {
      ctx = new AudioContext();
      src = ctx.createMediaStreamSource(this.stream);
    }
    await ctx.audioWorklet.addModule(URL.createObjectURL(new Blob([WORKLET], { type: "text/javascript" })));
    const node = new AudioWorkletNode(ctx, "mic");
    node.port.onmessage = (e) => onFrame(e.data.pcm, e.data.peak);
    let input = src;
    if (ctx.sampleRate !== 16000) {
      const lp = ctx.createBiquadFilter();
      lp.type = "lowpass";
      lp.frequency.value = 7000;
      src.connect(lp);
      input = lp;
    }
    input.connect(node);
    this.ctx = ctx;
  }

  stop() {
    this.stream?.getTracks().forEach((t) => t.stop());
    this.ctx?.close();
    this.stream = this.ctx = null;
  }
}

/** wav 把 16 kHz 单声道 PCM 帧编码为 WAV（模型直接听语音消息：火山方舟接受 wav、mp3、m4a，不接受 webm）。 */
export function wav(frames, rate = 16000) {
  const n = frames.reduce((s, f) => s + f.length, 0);
  const buf = new ArrayBuffer(44 + n * 2);
  const v = new DataView(buf);
  const str = (o, s) => [...s].forEach((c, i) => v.setUint8(o + i, c.charCodeAt(0)));
  str(0, "RIFF");
  v.setUint32(4, 36 + n * 2, true);
  str(8, "WAVE");
  str(12, "fmt ");
  v.setUint32(16, 16, true);
  v.setUint16(20, 1, true);
  v.setUint16(22, 1, true);
  v.setUint32(24, rate, true);
  v.setUint32(28, rate * 2, true);
  v.setUint16(32, 2, true);
  v.setUint16(34, 16, true);
  str(36, "data");
  v.setUint32(40, n * 2, true);
  let o = 44;
  for (const f of frames) for (const x of f) (v.setInt16(o, x, true), (o += 2));
  return new Uint8Array(buf);
}

/** Recorder 录一段语音消息：start 后 stop 返回 WAV 字节与时长（秒）。onLevel(peak) 用于显示音量。 */
export class Recorder {
  async start(onLevel) {
    this.frames = [];
    this.mic = new Mic();
    await this.mic.start((pcm, peak) => {
      this.frames.push(pcm);
      onLevel?.(peak);
    });
  }

  stop() {
    this.mic?.stop();
    const frames = this.frames ?? [];
    this.frames = [];
    return { bytes: wav(frames), seconds: frames.length * 0.02 };
  }
}

/** Player 播放通话的下行音频（PCM 16 位，按到达顺序排队）；stop 打断：停止已排队的音频，丢弃该回复迟到的音频。 */
export class Player {
  constructor() {
    this.ctx = new AudioContext();
    this.head = 0;
    this.sources = new Set();
    this.current = "";
    this.dropped = new Set();
  }

  /** pcm 为 base64（protojson 中的 bytes）。 */
  play(pcmB64, responseId, rate = 24000) {
    if (this.dropped.has(responseId)) return;
    this.current = responseId;
    const raw = Uint8Array.from(atob(pcmB64), (c) => c.charCodeAt(0));
    const pcm = new Int16Array(raw.buffer, 0, raw.length >> 1);
    const buf = this.ctx.createBuffer(1, pcm.length, rate);
    const ch = buf.getChannelData(0);
    for (let i = 0; i < pcm.length; i++) ch[i] = pcm[i] / 32768;
    const src = this.ctx.createBufferSource();
    src.buffer = buf;
    src.connect(this.ctx.destination);
    // 留 60 ms 余量，吸收网络抖动。
    const at = Math.max(this.ctx.currentTime + 0.06, this.head);
    src.start(at);
    this.head = at + buf.duration;
    this.sources.add(src);
    src.onended = () => this.sources.delete(src);
  }

  stop() {
    for (const s of this.sources) s.stop();
    this.sources.clear();
    this.head = 0;
    if (this.current) this.dropped.add(this.current);
  }

  close() {
    this.stop();
    this.ctx.close();
  }
}
