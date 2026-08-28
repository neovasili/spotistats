import { expect, test, type Page } from '@playwright/test'

/**
 * Collects console errors and failed requests for the life of a page.
 *
 * A React error boundary, a failed snapshot fetch or a chart that throws on real data all
 * produce a page that still returns HTTP 200. Watching the console is what turns those from
 * "looks fine" into a failure.
 */
function watchForErrors(page: Page): string[] {
  const problems: string[] = []
  page.on('console', (msg) => {
    if (msg.type() === 'error') problems.push(`console: ${msg.text()}`)
  })
  page.on('pageerror', (err) => problems.push(`pageerror: ${err.message}`))
  page.on('requestfailed', (req) => {
    // Image failures are expected and handled: most entities have no artwork yet, and the
    // renderer falls back to an initial tile. Everything else is a real problem.
    if (req.resourceType() !== 'image') {
      problems.push(`requestfailed: ${req.url()} ${req.failure()?.errorText ?? ''}`)
    }
  })
  return problems
}

test.describe('dashboard', () => {
  test('renders real figures, not an empty shell', async ({ page }) => {
    const problems = watchForErrors(page)
    await page.goto('/')

    // The hero is rendered from the snapshot, so a number here proves fetch + parse + mount.
    const hero = page.locator('.hero__value')
    await expect(hero).toBeVisible()
    await expect(hero).not.toHaveText(/^\s*0\s*hours\s*$/)

    // The KPI row and at least one chart card. The headline row is inventory only -- the streak
    // and the year total live in the Activity band now.
    await expect(page.locator('.headline .tile').first()).toBeVisible()
    await expect(page.locator('.card').first()).toBeVisible()

    expect(problems, `page reported errors:\n${problems.join('\n')}`).toEqual([])
  })

  test('leads with the listening activity heatmap', async ({ page }) => {
    await page.goto('/')
    // The order is a deliberate product decision (recent-first); this is the browser-level
    // guard beside the unit test.
    await expect(page.locator('.card__title').first()).toHaveText('Listening activity')
    // A populated heatmap, not an empty grid.
    expect(await page.locator('.heatmap__cell').count()).toBeGreaterThan(300)
  })

  test('shows a tooltip with listening time on a heatmap day', async ({ page }) => {
    await page.goto('/')
    const cell = page.locator('.heatmap__cell:not(.heatmap__cell--empty)').first()
    await cell.hover()
    const tip = page.locator('.tooltip')
    await expect(tip).toBeVisible()
    // Durations are shown twice everywhere; the minute form is the part a tooltip must carry.
    await expect(tip).toContainText(/\d/)
    // ...and the weekday, which is what a grid of days actually invites you to ask.
    await expect(tip.locator('.tooltip__label')).toHaveText(/Mon|Tue|Wed|Thu|Fri|Sat|Sun/)
  })

  test('states a per-occurrence average on the rhythm charts', async ({ page }) => {
    // A bucket total is unreadable as a habit: "4d 7h at 20:00" says nothing until you know it
    // accumulated over seventeen years.
    await page.goto('/')
    const card = page.locator('.card').filter({ has: page.getByText('By day of week', { exact: true }) })
    const col = card.locator('.column').nth(3)
    await col.scrollIntoViewIfNeeded()
    await col.hover()
    await expect(page.locator('.tooltip')).toContainText(
      /avg .+ per (Monday|Tuesday|Wednesday|Thursday|Friday|Saturday|Sunday)/,
    )
  })

  test('pairs cards two to a row, with the heatmap full width', async ({ page }) => {
    // The layout is a deliberate product decision, and a CSS regression is silent otherwise: the
    // grid would simply stack and nothing would look broken.
    await page.setViewportSize({ width: 1440, height: 900 })
    await page.goto('/')

    const pair = page.locator('.grid').first()
    const cards = pair.locator('> .card')
    await expect(cards).toHaveCount(2)
    await cards.first().scrollIntoViewIfNeeded()
    const left = (await cards.nth(0).boundingBox())!
    const right = (await cards.nth(1).boundingBox())!
    // Side by side, not stacked: same row, and the second starts after the first ends.
    expect(right.x).toBeGreaterThan(left.x + left.width - 1)
    expect(Math.abs(right.y - left.y)).toBeLessThan(4)

    // The heatmap cannot be halved -- a hundred-odd week columns do not fit -- so it stays out
    // of any pair and spans the page.
    const heatmapCard = page.locator('.card').filter({ has: page.locator('.heatmap') })
    const heatmapBox = (await heatmapCard.boundingBox())!
    expect(heatmapBox.width).toBeGreaterThan(left.width * 1.8)
  })

  test('collapses the pairs to one column on a narrow viewport', async ({ page }) => {
    await page.setViewportSize({ width: 800, height: 900 })
    await page.goto('/')
    const cards = page.locator('.grid').first().locator('> .card')
    await cards.first().scrollIntoViewIfNeeded()
    const top = (await cards.nth(0).boundingBox())!
    const bottom = (await cards.nth(1).boundingBox())!
    expect(bottom.y).toBeGreaterThan(top.y + top.height - 1)
    expect(Math.abs(bottom.x - top.x)).toBeLessThan(4)
  })

  test('flips the tooltip below a mark with no headroom, not over the card title', async ({ page }) => {
    // Placement is the one tooltip behaviour jsdom cannot check, so it lives here and only here.
    // The rule is `mark.top - plot.top < 64px`, which means the chart used to demonstrate it has
    // to have marks at DIFFERENT depths. The by-year columns did until that card became a list;
    // the rhythm columns cannot, because each one is full-height and so every top edge is the
    // plot's own. The heatmap can: its rows are 13px apart, so row 0 flips and row 6 does not.
    await page.goto('/')
    const week = page.locator('.heatmap__week').nth(30)
    await week.scrollIntoViewIfNeeded()
    const cells = week.locator('.heatmap__cell:not(.heatmap__cell--empty)')
    const card = page.locator('.card').filter({ has: page.locator('.heatmap') })

    await cells.first().hover()
    await expect(page.locator('.tooltip')).toHaveClass(/tooltip--below/)
    const tip = (await page.locator('.tooltip').boundingBox())!
    const title = (await card.locator('.card__title').boundingBox())!
    expect(tip.y).toBeGreaterThan(title.y + title.height)

    // The bottom row has plenty of headroom, so it must NOT flip -- otherwise the rule is not a
    // rule, it is an unconditional change of placement.
    await cells.last().hover()
    await expect(page.locator('.tooltip')).not.toHaveClass(/tooltip--below/)
  })

  test('states every year on its own row, beside the artist who owned it', async ({ page }) => {
    // The by-year card is a list read downward so that each year lands on the SAME LINE as that
    // year in the card beside it. Asserted in the browser because it is a claim about two
    // independent grids agreeing, which is exactly what a stylesheet edit breaks silently.
    await page.goto('/')
    const bars = page.locator('.yearbars__row')
    await bars.first().scrollIntoViewIfNeeded()
    expect(await bars.count()).toBeGreaterThan(1)

    const barYears = await page.locator('.yearbars__year').allTextContents()
    const listYears = await page.locator('.yearlist__year').allTextContents()
    expect(barYears).toEqual(listYears) // same years, same order, both newest-first

    // EVERY row, not just the first. Checking only the top row hid a 2.5px per-row difference
    // -- the list rows carry artwork and the bar rows do not -- which left the eighteenth year
    // 37px out of line while the first looked perfect.
    const drift = await page.evaluate(() => {
      const bars = [...document.querySelectorAll('.yearbars__row')]
      const list = [...document.querySelectorAll('.yearlist__row')]
      return bars.map((r, i) => Math.abs(r.getBoundingClientRect().y - list[i].getBoundingClientRect().y))
    })
    expect(Math.max(...drift), `row offsets: ${drift.map(Math.round).join(',')}`).toBeLessThan(4)
  })

  test('shows the recent-past tiles in one row, longest scale to shortest', async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 900 })
    await page.goto('/')
    const tiles = page.locator('.tiles--inline .tile')
    // allTextContents() and .all() do NOT auto-wait -- they return [] the instant the selector
    // misses. Over a CDN the snapshot has not arrived yet at that point, so without this the
    // assertion below reads an empty array and the heights loop below runs zero times and
    // "passes" having checked nothing.
    await expect(tiles.first()).toBeVisible()
    const labels = await tiles.locator('.tile__label').allTextContents()
    expect(labels.slice(0, 2)).toEqual(['Current streak', '2026'])
    // One row: every tile shares a top edge.
    const tops = await tiles.evaluateAll((els) =>
      [...new Set(els.map((e) => Math.round(e.getBoundingClientRect().y)))],
    )
    expect(tops).toHaveLength(1)
  })

  test('matches the heights of two cards sharing a row', async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 900 })
    await page.goto('/')
    // See the note above: .all() resolves immediately, so an unloaded page would iterate nothing.
    await expect(page.locator('.grid > .card').first()).toBeVisible()
    const pairs = await page.locator('.grid').all()
    expect(pairs.length).toBeGreaterThan(0)
    for (const pair of pairs) {
      const cards = pair.locator('> .card')
      await cards.first().scrollIntoViewIfNeeded()
      const hs = await cards.evaluateAll((els) => els.map((e) => Math.round(e.getBoundingClientRect().height)))
      expect(new Set(hs).size, `heights ${hs.join(' vs ')}`).toBe(1)
    }
  })

  test('pins a tooltip on click and releases it on Escape', async ({ page }) => {
    await page.goto('/')
    await page.locator('.heatmap__cell:not(.heatmap__cell--empty)').first().click()
    await expect(page.locator('.tooltip--pinned')).toBeVisible()
    await page.keyboard.press('Escape')
    await expect(page.locator('.tooltip')).toHaveCount(0)
  })

  test('renders artwork or a fallback tile, never a broken image', async ({ page }) => {
    await page.goto('/')
    const card = page.locator('.card', { hasText: 'Top artists' }).first()
    await expect(card).toBeVisible()
    // Every row has one or the other. A broken <img> would be neither.
    const rows = card.locator('.bar')
    const n = await rows.count()
    expect(n).toBeGreaterThan(0)
    for (let i = 0; i < Math.min(n, 5); i++) {
      const row = rows.nth(i)
      await expect(row.locator('.artwork')).toHaveCount(1)
    }
    // Thumbnails are loading="lazy" and this card sits well below the fold, so they have to
    // be brought into view before they load at all -- checking naturalWidth first would just
    // measure images the browser has correctly not fetched yet.
    await card.scrollIntoViewIfNeeded()

    // Any image that has FINISHED loading must have real pixels. A decoded-but-broken image is
    // the case the onError fallback exists for, and the one worth catching.
    await expect
      .poll(
        async () =>
          card.locator('img.artwork').evaluateAll((imgs) =>
            (imgs as HTMLImageElement[])
              .filter((i) => i.complete)
              .every((i) => i.naturalWidth > 0),
          ),
        { message: 'an <img> decoded with no pixels; the fallback should have taken over' },
      )
      .toBe(true)
  })

  test('links artwork back to Spotify, as the policy requires', async ({ page }) => {
    await page.goto('/')
    const link = page.locator('a[href^="https://open.spotify.com/"]').first()
    await expect(link).toHaveAttribute('rel', /noopener/)
    await expect(link).toHaveAttribute('target', '_blank')
    await expect(page.locator('.footer__attribution')).toContainText('Spotify')
  })

  test('cycles theme and actually repaints in dark', async ({ page }) => {
    await page.goto('/')
    const toggle = page.getByRole('button', { name: /^Theme:/ })
    const themeAttr = () => page.evaluate(() => document.documentElement.dataset.theme ?? 'system')

    // The toggle cycles system -> light -> dark -> system. Starting from `system`, ONE click
    // reaches `light`, which under Playwright's default light colour scheme paints identically
    // to where it started -- so a single click proves nothing about the styling.
    await expect(toggle).toHaveText('Auto')
    await toggle.click()
    await expect(toggle).toHaveText('Light')
    expect(await themeAttr()).toBe('light')

    const light = await page.evaluate(() => getComputedStyle(document.body).backgroundColor)
    await toggle.click()
    await expect(toggle).toHaveText('Dark')
    expect(await themeAttr()).toBe('dark')

    // Dark mode is a selected set of steps, not an inversion, so it must genuinely differ.
    await expect
      .poll(() => page.evaluate(() => getComputedStyle(document.body).backgroundColor))
      .not.toBe(light)

    // And back to system, which removes the attribute so the OS preference applies again.
    await toggle.click()
    await expect(toggle).toHaveText('Auto')
    expect(await themeAttr()).toBe('system')
  })
})

test.describe('explorer', () => {
  test('loads rows from the query API', async ({ page }) => {
    const problems = watchForErrors(page)
    await page.goto('/explore')

    // Rows come from /api/v1/list, so this proves the API, the CloudFront route and the
    // rendering all work together.
    await expect(page.locator('.datatable--interactive tbody tr').first()).toBeVisible()
    expect(await page.locator('.datatable--interactive tbody tr').count()).toBeGreaterThan(1)
    expect(problems, `page reported errors:\n${problems.join('\n')}`).toEqual([])
  })

  test('reproduces a shared query from the URL', async ({ page }) => {
    // The whole point of URL-backed filter state.
    await page.goto('/explore?dim=ARTIST&period=ALL&sort=ms&order=desc')
    await expect(page.getByRole('button', { name: 'Artists' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    await expect(page.locator('.card__sub').first()).toContainText('artists')
  })

  test('opens a drill-down with a monthly trend', async ({ page }) => {
    await page.goto('/explore?dim=ARTIST&period=2026')
    await page.locator('.datatable--interactive tbody .linkbutton').first().click()
    // The detail panel renders the entity's figures plus a trend for the selected year.
    await expect(page.locator('.detail__big').first()).toBeVisible()
    await expect(page.locator('.trend, .empty').first()).toBeVisible()

    // Hovering the BAR, not the label beneath it. The handlers used to sit on the label alone,
    // so this exact gesture did nothing. The check lived on the dashboard's by-year card until
    // that stopped being a Trend; this is the component's only remaining caller.
    // .hover() rather than a measured mouse.move: it targets the element's centre, which for a
    // 72px column is the middle of the bar and well clear of the label at its foot, and it
    // re-checks actionability instead of trusting a box measured while the panel was settling.
    const col = page.locator('.trend__col').nth(1)
    if (await col.count()) {
      await col.hover()
      await expect(page.locator('.tooltip')).toBeVisible()
    }
  })

  test('serves the deep link directly, not only via client navigation', async ({ page }) => {
    // /explore is not a file in the bucket; a CloudFront function rewrites it to index.html.
    const res = await page.goto('/explore')
    expect(res?.status()).toBe(200)
    await expect(page.getByRole('link', { name: 'Explorer' })).toHaveAttribute(
      'aria-current',
      'page',
    )
  })
})

test.describe('artist profile', () => {
  test('is reachable from a dashboard leaderboard row', async ({ page }) => {
    const problems = watchForErrors(page)
    await page.goto('/')

    // The artist name links inward to the profile; the artwork beside it still links out to
    // Spotify, which the Developer Policy requires.
    const link = page.locator('a[href^="/artist/"]').first()
    await expect(link).toBeVisible()
    await link.click()

    await expect(page).toHaveURL(/\/artist\//)
    // Either a resolved profile or the honest "no profile yet" -- both are correct outcomes,
    // and which one appears depends on how far enrichment has got.
    await expect(page.locator('.profile__name, .card__title').first()).toBeVisible()
    expect(problems, `page reported errors:\n${problems.join('\n')}`).toEqual([])
  })

  test('serves a profile deep link directly', async ({ page }) => {
    // /artist/<id> is not a file in the bucket either; the same CloudFront function rewrites it.
    const res = await page.goto('/artist/does-not-exist')
    expect(res?.status()).toBe(200)
    // An unknown id has no EXTERNAL row, so the API answers 404 and the page says so plainly
    // rather than showing a skeleton that implies data is on its way.
    await expect(page.getByText('No profile yet')).toBeVisible()
    await expect(page.locator('.empty', { hasText: 'Loading' })).toHaveCount(0)
  })

  test('never merges the two genre vocabularies', async ({ page }) => {
    await page.goto('/')
    await page.locator('a[href^="/artist/"]').first().click()
    await expect(page).toHaveURL(/\/artist\//)

    const rows = page.locator('.chiprow')
    if ((await rows.count()) === 0) return // this artist has no genres from either source

    // Every chip row carries its source label, so no chip's provenance is ambiguous.
    for (const row of await rows.all()) {
      await expect(row.locator('.chiprow__label')).toHaveText(/MusicBrainz|Spotify/)
    }
  })
})
