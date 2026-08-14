/// <reference types="vite/client" />

declare const __PIE_RELAY_URL__: string;

interface ImportMetaEnv {
  readonly VITE_PIE_RELAY_APPLICATION_ID?: string;
  readonly VITE_PIE_RELAY_POOL_ID?: string;
  readonly VITE_PIE_RELAY_TENANT_ID?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
