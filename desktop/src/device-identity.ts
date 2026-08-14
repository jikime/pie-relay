export interface LocalDeviceIdentity {
  id: string;
  name: string;
}

const DEVICE_KEY = "pie-relay.local-device";
const HOST_KEY = "pie-relay.host";

function generatedID(): string {
  const id =
    globalThis.crypto?.randomUUID?.() ??
    `${Date.now()}-${Math.random().toString(16).slice(2)}`;
  return `desktop-${id}`;
}

export function localDeviceIdentity(): LocalDeviceIdentity {
  const fallback = { id: generatedID(), name: "이 PC" };
  if (typeof localStorage === "undefined") return fallback;
  try {
    const saved = localStorage.getItem(DEVICE_KEY);
    if (saved) {
      const parsed = JSON.parse(saved) as Partial<LocalDeviceIdentity>;
      if (parsed.id) {
        return { id: parsed.id, name: parsed.name?.trim() || fallback.name };
      }
    }
    // Preserve the device identity already used by the legacy Host panel so a
    // UI upgrade does not register the same computer as two different devices.
    const host = localStorage.getItem(HOST_KEY);
    if (host) {
      const parsed = JSON.parse(host) as {
        deviceId?: string;
        deviceName?: string;
      };
      if (parsed.deviceId) {
        const migrated = {
          id: parsed.deviceId,
          name: parsed.deviceName?.trim() || fallback.name,
        };
        localStorage.setItem(DEVICE_KEY, JSON.stringify(migrated));
        return migrated;
      }
    }
    localStorage.setItem(DEVICE_KEY, JSON.stringify(fallback));
  } catch {
    // Private browsing or corrupt legacy settings: keep an in-memory identity.
  }
  return fallback;
}

export function isTauriDesktop(): boolean {
  return typeof window !== "undefined" && "__TAURI_INTERNALS__" in window;
}

