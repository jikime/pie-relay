// sidecar.ts — pure helpers for the host panel. Parsing of the Go client's
// stdout lives here (not in the Rust command or the React component) so it can
// be unit-tested with vitest, matching the design's "사이드카 출력 파싱 순수
// 함수는 vitest" rule.

// parseInviteCode extracts the invite code from `client room create` output.
// The Go client prints a line like "초대 코드: ABCD2345" (see
// ../../client/cmd/client/main.go runRoomCreate). Codes are uppercase
// alphanumeric; we accept 4+ chars to stay tolerant of length changes. Returns
// null when no code line is present (e.g. an error was printed instead).
export function parseInviteCode(output: string): string | null {
  for (const raw of output.split(/\r?\n/)) {
    const m = raw.match(/초대\s*코드\s*[:：]\s*([A-Za-z0-9]{4,})/);
    if (m) return m[1];
  }
  return null;
}

// ringPush appends a line to a bounded log buffer, dropping the oldest entries
// once `max` is exceeded. Returns a new array (never mutates the input) so React
// state updates stay predictable. Used for the 200-line daemon log view.
export function ringPush(buffer: string[], line: string, max: number): string[] {
  const next = buffer.length >= max ? buffer.slice(buffer.length - max + 1) : buffer.slice();
  next.push(line);
  return next;
}
