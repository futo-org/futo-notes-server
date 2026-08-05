const MAX_MUTATION_ID_LENGTH = 128
const MUTATION_ID_PATTERN = /^[A-Za-z0-9._~-]+$/

export function isValidMutationId(value: string): boolean {
  return value.length > 0 && value.length <= MAX_MUTATION_ID_LENGTH
}

export function isValidBatchMutationId(value: string): boolean {
  return isValidMutationId(value) && MUTATION_ID_PATTERN.test(value)
}
