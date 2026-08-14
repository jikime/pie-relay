import React from "react";
import ReactDOM from "react-dom/client";
import { App } from "./App";
import { loadRelayConfig } from "./relay-config";
import "./styles.css";

const root = ReactDOM.createRoot(document.getElementById("root") as HTMLElement);

void loadRelayConfig().then((relayConfig) => {
  root.render(
    <React.StrictMode>
      <App relayConfig={relayConfig} />
    </React.StrictMode>,
  );
}).catch((error: unknown) => {
  root.render(
    <main style={{ padding: 24, fontFamily: "sans-serif" }}>
      <h1>Pie Relay 설정 오류</h1>
      <p role="alert">{error instanceof Error ? error.message : String(error)}</p>
    </main>,
  );
});
