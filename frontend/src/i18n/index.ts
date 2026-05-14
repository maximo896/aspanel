import zh from './zh'

export function t(key: string): string {
  return zh[key] || key
}
