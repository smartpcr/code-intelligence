# SPDX-License-Identifier: CC0-1.0
from pkg import module_a

class BetaBase:
    def base(self):
        return 1

class BetaMid(BetaBase):
    def mid(self):
        return 2

class BetaLeaf(BetaMid):
    def leaf(self):
        return module_a.duplicate_one(3)

def beta_value():
    return module_a.duplicate_two(4)