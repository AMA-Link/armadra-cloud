import manifest from "../package.json" with { type: "json" };

export const RELAY_VERSION: string = manifest.version;
