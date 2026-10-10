// 移动端绑定层的冒烟测试，由 internal/e2e（TestMobileSwift）启动网关后运行：
//
//   YANSHI_URL=ws://…/v1/connect YANSHI_TOKEN=… .build/release/Smoke
//
// 同一条连接既是 Node（声明原生能力 take_note）又是会话客户端：创建 Session、订阅、提交 "call take_note"，
// 等到 Run 完成且助手看到能力的结果。成功时输出 "ok"。

import Foundation
import SwiftProtobuf
import Yanshi

let env = ProcessInfo.processInfo.environment
guard let url = env["YANSHI_URL"], let token = env["YANSHI_TOKEN"] else {
    fatalError("YANSHI_URL and YANSHI_TOKEN are required")
}

func check(_ cond: Bool, _ what: String) {
    if !cond {
        FileHandle.standardError.write("smoke: \(what)\n".data(using: .utf8)!)
        exit(1)
    }
}

final class Tokens: NSObject, YanshiTokenSourceProtocol {
    private let value: String
    init(_ value: String) { self.value = value }
    func token() -> String { value }
}

final class Connected: NSObject, YanshiConnectionObserverProtocol {
    let ready = DispatchSemaphore(value: 0)
    func onConnected(_ label: String?) { ready.signal() }
}

final class TakeNote: NSObject, YanshiCapabilityHandlerProtocol {
    func execute(_ invoke: Data?) throws -> Data {
        let inv = try Yanshi_V1_Invoke(serializedBytes: invoke ?? Data())
        var result = Yanshi_V1_InvokeResult()
        result.callID = inv.callID
        var block = Yanshi_V1_ContentBlock()
        block.text.text = "note from swift"
        result.content = [block]
        return try result.serializedData()
    }
}

/// 回调在 SDK 的线程中调用：用锁保护，测试线程轮询。
final class View: NSObject, YanshiSessionHandlerProtocol {
    private let lock = NSLock()
    private var events: [Yanshi_V1_Event] = []
    private var deltas = 0

    func onEvent(_ event: Data?) {
        guard let e = try? Yanshi_V1_Event(serializedBytes: event ?? Data()) else { return }
        lock.withLock { events.append(e) }
    }
    func onDelta(_ delta: Data?) { lock.withLock { deltas += 1 } }
    func onPresence(_ presence: Data?) {}
    func onEnded(_ reason: String?) {}

    var completed: Bool {
        lock.withLock { events.contains { if case .runCompleted = $0.payload { true } else { false } } }
    }
    var assistantText: String {
        lock.withLock {
            events.compactMap { e -> String? in
                if case .assistantMessage(let m) = e.payload { m.content.map(\.text.text).joined() } else { nil }
            }.last ?? ""
        }
    }
    var seqs: [UInt64] { lock.withLock { events.map(\.seq) } }
}

func request(_ client: YanshiClient, _ req: Yanshi_V1_ClientRequest) throws -> Yanshi_V1_ClientResponse {
    let resp = try Yanshi_V1_ClientResponse(serializedBytes: client.request(req.serializedData(), timeoutMillis: 5000))
    check(!resp.hasError, "request failed: \(resp.error.code) \(resp.error.message)")
    return resp
}

func until(_ what: String, _ cond: () -> Bool) {
    let deadline = Date().addingTimeInterval(10)
    while !cond() {
        check(Date() < deadline, "timed out waiting for \(what)")
        Thread.sleep(forTimeInterval: 0.01)
    }
}

let cfg = YanshiConfig()
cfg.url = url
cfg.nodeID = "iphone-swift"
cfg.label = "iPhone"
cfg.kind = "phone"
cfg.ledgerDir = FileManager.default.temporaryDirectory.appendingPathComponent("yanshi-smoke-\(getpid())").path
guard let client = YanshiClient(cfg) else { fatalError("no client") }
client.setTokenSource(Tokens(token))
let connected = Connected()
client.setObserver(connected)

var spec = Yanshi_V1_CapabilitySpec()
spec.name = "take_note"
spec.description_p = "记一条笔记"
spec.idempotent = true
spec.risk = .low
try client.addCapability(spec.serializedData(), handler: TakeNote())
try client.start()
check(connected.ready.wait(timeout: .now() + 5) == .success, "not connected")

var create = Yanshi_V1_ClientRequest()
create.createSession.agent = "dev"
let sid = try request(client, create).sessionID
check(!sid.isEmpty, "no session")

let view = View()
let sub = try client.subscribe(sid, afterSeq: 0, handler: view)
client.setActivity(sid, focused: true, typing: false)

let submitted = try Yanshi_V1_ClientResponse(serializedBytes: client.submitText(sid, text: "call take_note", timeoutMillis: 5000))
check(!submitted.runID.isEmpty, "submit returned no run")
until("the run completes", { view.completed })
check(view.assistantText == "done: note from swift", "assistant saw \(view.assistantText)")
check(view.seqs == Array(1...UInt64(view.seqs.count)), "events are not contiguous: \(view.seqs)")

// 网关拒绝的请求以响应中的 error 表示。
var decide = Yanshi_V1_ClientRequest()
decide.decide.sessionID = "nope"
decide.decide.callID = "x"
let rejected = try Yanshi_V1_ClientResponse(serializedBytes: client.request(decide.serializedData(), timeoutMillis: 5000))
check(rejected.error.code == "not_found", "decide on a missing session: \(rejected)")

sub.cancel()
client.stop()
print("ok")
