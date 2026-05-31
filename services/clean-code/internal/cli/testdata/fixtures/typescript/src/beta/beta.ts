// SPDX-License-Identifier: CC0-1.0
import { duplicateOne } from "src/alpha";

export class BetaBase { base(): number { return 1; } }
export class BetaMid extends BetaBase { mid(): number { return 2; } }
export class BetaLeaf extends BetaMid { leaf(): number { return duplicateOne(3); } }

export function betaValue(): number { return duplicateOne(4); }