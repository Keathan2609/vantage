// ESLint flat configuration.
//
// Next 16 removed `next lint`, so linting runs ESLint directly. The Next
// config is a flat-config array and is spread in as-is; the additions below
// are the project's own rules.

import next from "eslint-config-next/core-web-vitals";

const config = [
  {
    ignores: [
      ".next/**",
      "node_modules/**",
      "next-env.d.ts",
      // Playwright specs run under a different tsconfig and are typechecked
      // by the Playwright runner, not by the app's build.
      "tests/e2e/**",
    ],
  },
  ...next,
  {
    rules: {
      // NOTE: there is deliberately no lint rule banning Number.parseFloat.
      //
      // An earlier revision had one, written against the bare `parseFloat`
      // global -- which this codebase never uses, so the rule never fired and
      // read as assurance it did not provide.
      //
      // The real constraint is not "never parse a float", it is "never let a
      // parsed float reach authoritative state". The 46 call sites are chart
      // coordinates, meter widths and percentage labels: presentational
      // quantities that never return to the server. Every submitted field is
      // the decimal STRING taken straight from its input.
      //
      // That property is enforced where it can be enforced: TypeScript types
      // every money field in lib/api.ts as `string`, and the Go side has an
      // architecture test asserting no floating-point column exists in the
      // schema. See internal/arch/arch_test.go.
    },
  },
];

export default config;
