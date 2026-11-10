import { describe, it, expect } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import App from '../src/App.vue'
import { jsonResponse, mockFetch } from './helpers'

const Empty = { template: '<div />' }

async function at(path) {
  mockFetch({ 'GET /healthz': () => jsonResponse(200, { status: 'ok', db: 'ok' }) })
  const router = createRouter({
    history: createMemoryHistory(),
    routes: ['/', '/new', '/campaigns/:id', '/monitoring', '/logs'].map((p) => ({ path: p, component: Empty })),
  })
  router.push(path)
  await router.isReady()
  const w = mount(App, { global: { plugins: [router] } })
  await flushPromises()
  return w
}

const current = (w) => w.findAll('nav a.nav.current').map((a) => a.text().replace(/\(.*\)/, '').trim())

describe('top navigation', () => {
  it('links to the campaign list and monitoring', async () => {
    const w = await at('/')
    const links = w.findAll('nav a.nav')
    expect(links.map((a) => a.attributes('href'))).toEqual(['/', '/monitoring'])
    expect(links[0].text()).toBe('Kampanyalar')
  })

  it.each([
    ['/', ['Kampanyalar']],
    ['/campaigns/7', ['Kampanyalar']],
    ['/monitoring', ['İzleme']],
    ['/logs', ['İzleme']],
    ['/new', []],
  ])('marks the current section on %s', async (path, expected) => {
    expect(current(await at(path))).toEqual(expected)
  })
})
