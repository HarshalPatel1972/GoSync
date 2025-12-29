import { get, set, entries } from './node_modules/idb-keyval/dist/index.js';

window.GoSyncDB = {
    async save(key, value) {
        await set(key, value);
    },
    async getAll() {
        const allEntries = await entries();
        // entries return [ [key1, val1], [key2, val2] ]
        // We just want values
        return allEntries.map(entry => entry[1]);
    }
};
