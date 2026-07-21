// test-setup.ts wires the two pieces every component test in this
// suite depends on: the testing-library/svelte auto cleanup hook (so
// one test's rendered DOM never leaks into the next) and the
// jest-dom matchers (toBeInTheDocument, toHaveTextContent, etc.) used
// by the assertions in src/routes/*.test.ts. Registered once here via
// vite.config.ts's test.setupFiles rather than per-test-file.
import '@testing-library/jest-dom/vitest'
import '@testing-library/svelte/vitest'
