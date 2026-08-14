// Node test shim for the small React Native surface used by the vendored
// transport modules. UI modules are not imported by the host gateway tests.
export const Platform = { OS: 'web', select: <T>(values: Record<string, T>) => values.web ?? values.default }
