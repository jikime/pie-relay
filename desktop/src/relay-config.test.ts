import { describe, expect, it } from "vitest";
import {
  DEFAULT_RELAY_ORIGIN,
  resolveRelayConfig,
} from "./relay-config";

describe("resolveRelayConfig", () => {
  it("defaults to the CookAI Relay", () => {
    expect(resolveRelayConfig()).toMatchObject({
      httpOrigin: DEFAULT_RELAY_ORIGIN,
      wsOrigin: DEFAULT_RELAY_ORIGIN.replace("https://", "wss://"),
    });
  });

  it("uses the same URL setting for a local Relay", () => {
    expect(resolveRelayConfig("http://127.0.0.1:13412")).toMatchObject({
      httpOrigin: "http://127.0.0.1:13412",
      agentUrl: "ws://127.0.0.1:13412/ws/agent",
    });
  });

  it("normalizes an agent websocket URL to its Relay origin", () => {
    expect(resolveRelayConfig("wss://relay.example/ws/agent?ignored=1")).toMatchObject({
      httpOrigin: "https://relay.example",
      participantUrl: "wss://relay.example",
    });
  });

  it("rejects unsupported schemes", () => {
    expect(() => resolveRelayConfig("file:///tmp/relay")).toThrow(
      "http(s) 또는 ws(s)",
    );
  });
});
