import { invoke } from "@tauri-apps/api/core";

export const DEFAULT_RELAY_ORIGIN =
  "https://relay.cookai.dev";

export interface RelayConfig {
  httpOrigin: string;
  wsOrigin: string;
  agentUrl: string;
  participantUrl: string;
  storageKey: string;
}

interface RelayEnvironment {
  relayUrl?: string;
}

function nonEmpty(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() !== ""
    ? value.trim()
    : undefined;
}

export function resolveRelayConfig(relayUrl?: string): RelayConfig {
  const configured = nonEmpty(relayUrl) || DEFAULT_RELAY_ORIGIN;
  let parsed: URL;
  try {
    parsed = new URL(configured);
  } catch {
    throw new Error(`Relay URL이 올바르지 않습니다: ${configured}`);
  }
  if (!["http:", "https:", "ws:", "wss:"].includes(parsed.protocol)) {
    throw new Error("Relay URL은 http(s) 또는 ws(s)를 사용해야 합니다.");
  }

  const secure = parsed.protocol === "https:" || parsed.protocol === "wss:";
  const httpOrigin = `${secure ? "https" : "http"}://${parsed.host}`;
  const wsOrigin = `${secure ? "wss" : "ws"}://${parsed.host}`;
  return {
    httpOrigin,
    wsOrigin,
    agentUrl: `${wsOrigin}/ws/agent`,
    participantUrl: wsOrigin,
    // Changing PIE_RELAY_URL invalidates credentials bound to the old Relay.
    storageKey: httpOrigin,
  };
}

function buildRelayUrl(): string | undefined {
  return __PIE_RELAY_URL__;
}

function isTauri(): boolean {
  return typeof window !== "undefined" && "__TAURI_INTERNALS__" in window;
}

// Packaged Tauri apps read PIE_RELAY_URL at runtime. Browser/Vite builds inject
// the same variable at build/dev-server startup. Unset means the CookAI Relay.
export async function loadRelayConfig(): Promise<RelayConfig> {
  const buildUrl = buildRelayUrl();
  if (!isTauri()) return resolveRelayConfig(buildUrl);

  try {
    const runtime = await invoke<RelayEnvironment>("relay_url_environment");
    return resolveRelayConfig(nonEmpty(runtime.relayUrl) || buildUrl);
  } catch (error) {
    console.warn("Relay runtime URL을 읽지 못해 build 설정을 사용합니다.", error);
    return resolveRelayConfig(buildUrl);
  }
}
