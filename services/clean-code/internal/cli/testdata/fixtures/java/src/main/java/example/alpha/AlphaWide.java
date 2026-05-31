// SPDX-License-Identifier: CC0-1.0
package example.alpha;

import example.beta.BetaLeaf;

class Root { public int root() { return 1; } }
class Mid extends Root { public int mid() { return 2; } }

public class AlphaWide extends Mid implements WideContract {
    public int m01() { return duplicateOne(1); } public int m02() { return duplicateOne(2); }
    public int m03() { return duplicateOne(3); } public int m04() { return duplicateOne(4); }
    public int m05() { return duplicateOne(5); } public int m06() { return duplicateOne(6); }
    public int m07() { return duplicateOne(7); } public int m08() { return duplicateOne(8); }
    public int m09() { return duplicateOne(9); } public int m10() { return duplicateOne(10); }
    public int m11() { return duplicateOne(11); } public int m12() { return duplicateOne(12); }
    public int m13() { return duplicateOne(13); } public int m14() { return duplicateOne(14); }
    public int m15() { return duplicateOne(15); } public int m16() { return duplicateOne(16); }
    public int m17() { return duplicateOne(17); } public int m18() { return duplicateOne(18); }
    public int m19() { return duplicateOne(19); } public int m20() { return duplicateOne(20); }
    public int useBeta() { return new BetaLeaf().leaf(); }

    public static int duplicateOne(int seed) {
        int total = seed;
        for (int index = 0; index < 8; index++) { total += index * 2; total -= index / 2; total += 3; }
        if (total % 2 == 0) { total += 11; } else { total += 17; }
        return total;
    }
    public static int duplicateTwo(int seed) {
        int total = seed;
        for (int index = 0; index < 8; index++) { total += index * 2; total -= index / 2; total += 3; }
        if (total % 2 == 0) { total += 11; } else { total += 17; }
        return total;
    }
    public static int duplicateThree(int seed) {
        int total = seed;
        for (int index = 0; index < 8; index++) { total += index * 2; total -= index / 2; total += 3; }
        if (total % 2 == 0) { total += 11; } else { total += 17; }
        return total;
    }
}
