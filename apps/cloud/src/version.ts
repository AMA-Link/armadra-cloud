import manifest from "../package.json" with { type: "json" };

export const CLOUD_VERSION: string = manifest.version;
