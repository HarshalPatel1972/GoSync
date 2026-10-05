export type SyncStatus = 'offline' | 'connecting' | 'online';

/** Any JSON-serialisable value. */
export type JSONValue = string | number | boolean | null | JSONValue[] | { [key: string]: JSONValue };

/** A stored document: its fields plus its `id`. */
export type Doc<T extends Record<string, JSONValue> = Record<string, JSONValue>> = T & { id: string };

export interface ClientOptions {
  /** Sync endpoint, e.g. "wss://sync.example.com/sync". */
  url: string | URL;
  /**
   * Returns the auth token (usually a JWT from your identity provider).
   * Called on every (re)connect, so return a fresh token when it expires.
   */
  getToken?: () => string | Promise<string>;
  /** Static token, if you do not need refresh. */
  token?: string;
  /** IndexedDB database name. Use one per signed-in user. Default "gosync". */
  dbName?: string;
  /** Where to load gosync.wasm from. Default: next to this module. */
  wasmUrl?: string | URL;
  /**
   * Only pull these collections from the server (default: all). Writes to
   * any collection are still pushed. All tabs sharing a dbName must use the
   * same list.
   */
  collections?: string[];
  /** Log sync activity to the console. */
  debug?: boolean;
}

export interface ChangeEvent {
  collection: string;
  ids: string[];
}

export interface GoSyncClient {
  readonly clientId: string;
  readonly status: SyncStatus;
  /** True in the one tab that holds the server connection. */
  readonly isLeader: boolean;

  /**
   * Creates or updates a document. Only the given fields change; set a field
   * to null to clear it. Works offline; syncs when connected.
   */
  set(collection: string, id: string, fields: Record<string, JSONValue>): Promise<void>;
  /** Deletes a document. A later set() revives it. */
  delete(collection: string, id: string): Promise<void>;
  get<T extends Record<string, JSONValue>>(collection: string, id: string): Promise<Doc<T> | null>;
  list<T extends Record<string, JSONValue>>(collection: string): Promise<Doc<T>[]>;

  /** Calls back when documents change (locally, in another tab, or from the server). */
  subscribe(callback: (e: ChangeEvent) => void): () => void;
  subscribe(collection: string, callback: (e: ChangeEvent) => void): () => void;
  /** Calls back with the full collection now and after every change. */
  watch<T extends Record<string, JSONValue>>(collection: string, callback: (docs: Doc<T>[]) => void): () => void;
  onStatus(callback: (status: SyncStatus) => void): () => void;
  /** Server errors (e.g. unauthorized, rejected writes). */
  onError(callback: (error: Error) => void): () => void;

  close(): void;
}

export function createClient(options: ClientOptions): Promise<GoSyncClient>;
