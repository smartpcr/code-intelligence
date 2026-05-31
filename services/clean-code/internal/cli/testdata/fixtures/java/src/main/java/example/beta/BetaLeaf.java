// SPDX-License-Identifier: CC0-1.0
package example.beta;

import example.alpha.AlphaWide;

class BetaBase { public int base() { return 1; } }
class BetaMid extends BetaBase { public int mid() { return 2; } }

public class BetaLeaf extends BetaMid {
    public int leaf() { return AlphaWide.duplicateOne(3); }
}
