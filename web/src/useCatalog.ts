import { useEffect, useState } from 'react'
import { getGpus } from './api'
import type { Pool } from './types'

// Undefined while loading or on failure; pages still render from stored runs.
export function useCatalog(): Pool[] | undefined {
  const [catalog, setCatalog] = useState<Pool[] | undefined>(undefined)
  useEffect(() => {
    let current = true
    getGpus()
      .then((c) => current && setCatalog(c))
      .catch(() => current && setCatalog(undefined))
    return () => {
      current = false
    }
  }, [])
  return catalog
}
