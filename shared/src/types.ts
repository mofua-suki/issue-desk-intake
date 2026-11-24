// 変更範囲の宣言（受付AIの出力を正規化したもの）と、Issue本文から読んだ宣言。
export type ScopeDeclaration = {
  known: boolean;
  paths: string[];
  reason: string;
  none?: boolean;
  backfilled?: boolean;
};
export type ParsedScope =
  | { state: 'absent' }
  | { state: 'unknown'; reason: string }
  | { state: 'declared'; paths: string[]; none?: true; backfilled?: true };

// 種類ごとに項目の違う値（判別できる共用体）を、項目を読むときは全部の種類の項目を持つものとして扱う。
export type VariantFields<T> = T & Record<string, any>;
