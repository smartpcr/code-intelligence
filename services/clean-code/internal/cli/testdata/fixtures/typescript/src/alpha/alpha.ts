// SPDX-License-Identifier: CC0-1.0
import { betaValue } from "src/beta";

export interface WideContract {
  m01(): number; m02(): number; m03(): number; m04(): number; m05(): number;
  m06(): number; m07(): number; m08(): number; m09(): number; m10(): number;
  m11(): number; m12(): number; m13(): number; m14(): number; m15(): number;
  m16(): number; m17(): number; m18(): number; m19(): number; m20(): number;
}

export class Root { root(): number { return 1; } }
export class Mid extends Root { mid(): number { return 2; } }
export class AlphaWide extends Mid implements WideContract {
  m01(): number { return duplicateOne(1); } m02(): number { return duplicateOne(2); }
  m03(): number { return duplicateOne(3); } m04(): number { return duplicateOne(4); }
  m05(): number { return duplicateOne(5); } m06(): number { return duplicateOne(6); }
  m07(): number { return duplicateOne(7); } m08(): number { return duplicateOne(8); }
  m09(): number { return duplicateOne(9); } m10(): number { return duplicateOne(10); }
  m11(): number { return duplicateOne(11); } m12(): number { return duplicateOne(12); }
  m13(): number { return duplicateOne(13); } m14(): number { return duplicateOne(14); }
  m15(): number { return duplicateOne(15); } m16(): number { return duplicateOne(16); }
  m17(): number { return duplicateOne(17); } m18(): number { return duplicateOne(18); }
  m19(): number { return duplicateOne(19); } m20(): number { return duplicateOne(20); }
}

export function useBeta(): number { return betaValue(); }

export function duplicateOne(seed: number): number {
  let total = seed;
  for (let index = 0; index < 8; index++) { total += index * 2; total -= Math.floor(index / 2); total += 3; }
  if (total % 2 === 0) { total += 11; } else { total += 17; }
  return total;
}
export function duplicateTwo(seed: number): number {
  let total = seed;
  for (let index = 0; index < 8; index++) { total += index * 2; total -= Math.floor(index / 2); total += 3; }
  if (total % 2 === 0) { total += 11; } else { total += 17; }
  return total;
}
export function duplicateThree(seed: number): number {
  let total = seed;
  for (let index = 0; index < 8; index++) { total += index * 2; total -= Math.floor(index / 2); total += 3; }
  if (total % 2 === 0) { total += 11; } else { total += 17; }
  return total;
}