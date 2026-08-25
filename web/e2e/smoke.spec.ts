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

    // The KPI row and at least one chart card.
    await expect(page.locator('.tiles .tile').first()).toBeVisible()
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
  })

  test('shows the by-year tooltip from the bar itself, not only the axis label', async ({ page }) => {
    await page.goto('/')
    const card = page.locator('.card').filter({ has: page.getByText('Listening by year', { exact: true }) })
    const col = card.locator('.trend__col').nth(2)
    // mouse.move takes viewport coordinates, so the card has to be on screen before it is measured.
    await col.scrollIntoViewIfNeeded()
    const box = (await col.boundingBox())!
    // The MIDDLE of the column, well clear of the year label at its foot: the handlers used to be
    // on the label alone, so this exact gesture did nothing.
    await page.mouse.move(box.x + box.width / 2, box.y + box.height * 0.4)
    await expect(page.locator('.tooltip')).toBeVisible()
  })

  test('flips the tooltip below a full-height bar rather than over the card title', async ({ page }) => {
    await page.goto('/')
    const card = page.locator('.card').filter({ has: page.getByText('Listening by year', { exact: true }) })
    const cols = card.locator('.trend__col')
    await cols.first().scrollIntoViewIfNeeded()
    // Find the tallest bar: its top edge is the top of the plot, so upward is off the chart.
    const heights = await cols.locator('.trend__fill').evaluateAll((els) =>
      els.map((e) => e.getBoundingClientRect().height),
    )
    const tallest = heights.indexOf(Math.max(...heights))
    const shortest = heights.indexOf(Math.min(...heights.filter((h) => h > 4)))

    await cols.nth(tallest).hover()
    await expect(page.locator('.tooltip')).toHaveClass(/tooltip--below/)
    const tip = (await page.locator('.tooltip').boundingBox())!
    const title = (await card.locator('.card__title').boundingBox())!
    expect(tip.y).toBeGreaterThan(title.y + title.height)

    // A short bar has plenty of headroom, so it must NOT flip -- otherwise the rule is not a
    // rule, it is an unconditional change of placement.
    await cols.nth(shortest).hover()
    await expect(page.locator('.tooltip')).not.toHaveClass(/tooltip--below/)
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
