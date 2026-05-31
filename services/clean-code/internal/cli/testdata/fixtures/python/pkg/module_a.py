# SPDX-License-Identifier: CC0-1.0
from pkg import module_b

class Root:
    def root(self):
        return "root"

class Mid(Root):
    def mid(self):
        return "mid"

class AlphaWide(Mid):
    def m01(self): return duplicate_one(1)
    def m02(self): return duplicate_one(2)
    def m03(self): return duplicate_one(3)
    def m04(self): return duplicate_one(4)
    def m05(self): return duplicate_one(5)
    def m06(self): return duplicate_one(6)
    def m07(self): return duplicate_one(7)
    def m08(self): return duplicate_one(8)
    def m09(self): return duplicate_one(9)
    def m10(self): return duplicate_one(10)
    def m11(self): return duplicate_one(11)
    def m12(self): return duplicate_one(12)
    def m13(self): return duplicate_one(13)
    def m14(self): return duplicate_one(14)
    def m15(self): return duplicate_one(15)
    def m16(self): return duplicate_one(16)
    def m17(self): return duplicate_one(17)
    def m18(self): return duplicate_one(18)
    def m19(self): return duplicate_one(19)
    def m20(self): return duplicate_one(20)

def use_beta():
    return module_b.beta_value()

def duplicate_one(seed):
    total = seed
    for index in range(8):
        total += index * 2
        total -= index // 2
        total += 3
    if total % 2 == 0:
        total += 11
    else:
        total += 17
    return total

def duplicate_two(seed):
    total = seed
    for index in range(8):
        total += index * 2
        total -= index // 2
        total += 3
    if total % 2 == 0:
        total += 11
    else:
        total += 17
    return total

def duplicate_three(seed):
    total = seed
    for index in range(8):
        total += index * 2
        total -= index // 2
        total += 3
    if total % 2 == 0:
        total += 11
    else:
        total += 17
    return total