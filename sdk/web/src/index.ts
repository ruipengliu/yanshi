// @yanshi/client：yanshi 的网页与 JavaScript 会话客户端（docs/design/m3-duplex-channel.md）。

export * from "./client.js";
export * from "./view.js";
// 契约中的消息类型（由 proto/ 生成，make gen）。
export * from "./gen/yanshi/v1/event_pb.js";
export * from "./gen/yanshi/v1/node_pb.js";
