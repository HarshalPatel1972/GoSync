import { get, set, entries } from './idb-keyval.js';

window.GoSyncDB = {
    async save(key, value) {
        await set(key, value);
        console.log(`[GoSyncDB] Saved items`);
    },
    async getAll() {
        const allEntries = await entries();
        console.log(`[GoSyncDB] Loaded ${allEntries.length} items`);
        // entries return [ [key1, val1], [key2, val2] ]
        // We just want values
        return allEntries.map(entry => entry[1]);
    }
};

console.log("GoSyncDB Implementation Loaded");
