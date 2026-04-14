/**
 * BlobStore — opaque key/value storage for encrypted blobs.
 *
 * The server never inspects blob contents. Implementations must treat
 * `data` as an opaque byte sequence and return it unchanged on `get`.
 */
export interface BlobStore {
  put(key: string, data: Uint8Array): Promise<void>
  get(key: string): Promise<Uint8Array | null>
  delete(key: string): Promise<void>
  list(prefix: string): Promise<string[]>
}
