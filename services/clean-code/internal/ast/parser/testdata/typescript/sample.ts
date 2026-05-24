// Fixture module for the clean-code v1 TypeScript parser
// adapter. Stage 2.1 tests assert the parser extracts the
// canonical AST scopes from this file (file, one interface,
// one class with three methods, one free function).

import { strict as assert } from "node:assert";

export interface Sampler {
    sample(seed: number): string;
    close(): void;
}

export class MemorySampler implements Sampler {
    private values: string[];
    private closed: boolean;

    constructor(initial: string[] = []) {
        this.values = [...initial];
        this.closed = false;
    }

    public sample(seed: number): string {
        assert(!this.closed, "sample: already closed");
        if (this.values.length === 0) {
            throw new Error("sample: empty buffer");
        }
        return this.values[seed % this.values.length];
    }

    public close(): void {
        this.closed = true;
    }
}

export function makeSampler(initial: string[] = []): MemorySampler {
    return new MemorySampler(initial);
}
