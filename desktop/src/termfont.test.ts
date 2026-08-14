import { describe, it, expect } from "vitest";
import {
  loadTermFont,
  serializeTermFont,
  TERM_FONTS,
  TERM_SIZES,
  DEFAULT_TERM_FONT,
} from "./termfont";

describe("TERM_FONTS / TERM_SIZES — option lists", () => {
  it("defaults to bundled D2Coding as the first option", () => {
    expect(TERM_FONTS[0].label).toBe("D2Coding");
    expect(TERM_FONTS[0].family).toContain('"D2Coding"');
    expect(DEFAULT_TERM_FONT.family).toBe(TERM_FONTS[0].family);
    expect(DEFAULT_TERM_FONT.size).toBe(13);
  });

  it("keeps D2Coding as a CJK fallback in every non-bundled stack", () => {
    for (const opt of TERM_FONTS) {
      expect(opt.family).toContain("D2Coding");
    }
  });

  it("offers sizes 11–16 including the default 13", () => {
    expect(TERM_SIZES).toContain(11);
    expect(TERM_SIZES).toContain(16);
    expect(TERM_SIZES).toContain(DEFAULT_TERM_FONT.size);
  });
});

describe("loadTermFont — parsing persisted values", () => {
  it("returns the default for null (unset)", () => {
    expect(loadTermFont(null)).toEqual(DEFAULT_TERM_FONT);
  });

  it("returns the default for non-JSON garbage", () => {
    expect(loadTermFont("not json{")).toEqual(DEFAULT_TERM_FONT);
  });

  it("returns the default for a non-object JSON value", () => {
    expect(loadTermFont("42")).toEqual(DEFAULT_TERM_FONT);
    expect(loadTermFont("null")).toEqual(DEFAULT_TERM_FONT);
  });

  it("round-trips a valid selection", () => {
    const font = { family: TERM_FONTS[1].family, size: 16 };
    expect(loadTermFont(serializeTermFont(font))).toEqual(font);
  });

  it("falls back the family independently when unknown", () => {
    const raw = JSON.stringify({ family: "Comic Sans", size: 14 });
    expect(loadTermFont(raw)).toEqual({
      family: DEFAULT_TERM_FONT.family,
      size: 14,
    });
  });

  it("falls back the size independently when out of range", () => {
    const raw = JSON.stringify({ family: TERM_FONTS[2].family, size: 99 });
    expect(loadTermFont(raw)).toEqual({
      family: TERM_FONTS[2].family,
      size: DEFAULT_TERM_FONT.size,
    });
  });

  it("ignores wrong-typed fields", () => {
    const raw = JSON.stringify({ family: 123, size: "14" });
    expect(loadTermFont(raw)).toEqual(DEFAULT_TERM_FONT);
  });
});
