import type { ReactNode, SVGProps } from "react";

export type UiIconName =
  | "arrow-left"
  | "chat"
  | "close"
  | "host"
  | "mobile"
  | "plus"
  | "shield"
  | "terminal"
  | "users"
  | "wifi";

interface Props extends Omit<SVGProps<SVGSVGElement>, "name"> {
  name: UiIconName;
  size?: number;
}

// Small, dependency-free line icons keep the desktop chrome crisp at compact
// sizes. They intentionally inherit currentColor so status/active states remain
// controlled by CSS rather than a second icon theme.
export function UiIcon({ name, size = 16, ...props }: Props) {
  const paths: Record<UiIconName, ReactNode> = {
    "arrow-left": (
      <>
        <path d="m15 18-6-6 6-6" />
        <path d="M9 12h11" />
      </>
    ),
    chat: (
      <path d="M21 15a4 4 0 0 1-4 4H8l-5 3V7a4 4 0 0 1 4-4h10a4 4 0 0 1 4 4Z" />
    ),
    close: (
      <>
        <path d="m8 8 8 8" />
        <path d="m16 8-8 8" />
      </>
    ),
    host: (
      <>
        <rect x="3" y="4" width="18" height="6" rx="2" />
        <rect x="3" y="14" width="18" height="6" rx="2" />
        <path d="M7 7h.01M7 17h.01" />
      </>
    ),
    mobile: (
      <>
        <rect x="7" y="2" width="10" height="20" rx="2" />
        <path d="M11 18h2" />
      </>
    ),
    plus: (
      <>
        <path d="M12 5v14" />
        <path d="M5 12h14" />
      </>
    ),
    shield: <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10Z" />,
    terminal: (
      <>
        <path d="m4 17 6-6-6-6" />
        <path d="M12 19h8" />
      </>
    ),
    users: (
      <>
        <path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2" />
        <circle cx="9" cy="7" r="4" />
        <path d="M22 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75" />
      </>
    ),
    wifi: (
      <>
        <path d="M5 12.55a11 11 0 0 1 14.08 0" />
        <path d="M8.53 16.11a6 6 0 0 1 6.95 0" />
        <path d="M12 20h.01" />
      </>
    ),
  };

  return (
    <svg
      viewBox="0 0 24 24"
      width={size}
      height={size}
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
      {...props}
    >
      {paths[name]}
    </svg>
  );
}
