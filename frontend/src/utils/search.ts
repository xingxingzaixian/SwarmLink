/**
 * 搜索用到的大小写规则。
 *
 * 唯一的硬约束：必须与后端 SQLite 的 LIKE 逐字一致 ——【只对 ASCII 字母】
 * 忽略大小写，其余字节精确比较。
 *
 * 这里单独成模块的理由是它【两端都要用】：
 *   - mock.ts 用它模拟后端的 LIKE（否则预览模式与真机在大小写上给出不同答案）；
 *   - SearchPanel 用它定位要高亮的那一处子串（后端只回原文，切片在前端做）。
 *
 * 用 `toLowerCase()` 是不行的：它对非 ASCII 也做折叠（İ/ı、K 的 Kelvin 符号
 * 一类），于是「同一个词」在预览里命中、在真机里不命中 —— 这类差异只在
 * 土耳其语系的字符上出现，恰恰最难被测到。
 */

/** ASCII 大写 → 小写；其余码位原样返回。 */
function foldAscii(c: number): number {
  return c >= 65 && c <= 90 ? c + 32 : c
}

/** 两个 UTF-16 码元是否等价（仅 ASCII 字母忽略大小写）。 */
function equalFoldAscii(a: number, b: number): boolean {
  return a === b || foldAscii(a) === foldAscii(b)
}

/**
 * needle 在 haystack 中首次出现的下标（找不到返回 -1）；空 needle 返回 -1。
 *
 * 逐码元比较在这里是安全的：两侧都是合法 UTF-8 解码出来的字符串，
 * 匹配不可能落在代理对（surrogate pair）的中间。
 */
export function indexOfFoldAscii(haystack: string, needle: string): number {
  const n = needle.length
  if (n === 0) return -1
  outer: for (let i = 0; i + n <= haystack.length; i++) {
    for (let j = 0; j < n; j++) {
      if (!equalFoldAscii(haystack.charCodeAt(i + j), needle.charCodeAt(j))) continue outer
    }
    return i
  }
  return -1
}

/** 子串包含（语义等价于后端的 LIKE '%needle%' 去掉通配符转义之后的样子）。 */
export function likeContains(haystack: string, needle: string): boolean {
  return needle === '' || indexOfFoldAscii(haystack, needle) >= 0
}
