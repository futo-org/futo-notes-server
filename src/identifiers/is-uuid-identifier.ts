const UUID_IDENTIFIER_PATTERN =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

export function isUuidIdentifier(value: string): boolean {
  return UUID_IDENTIFIER_PATTERN.test(value)
}
