// ESLint flat config for the packages/* workspaces.
//
// Adapted from Alpha's client/desktop/eslint.config.mjs, which linted this code
// before it moved here (epic #3947). The packages came from src/renderer and
// src/shared, so the renderer rule block applies to all of packages/*/src.
// Multirepo adaptations, and nothing else:
//   - no @eslint-react preset or rules: the packages hold no React code;
//   - no src/main block: the Electron main process stayed in the desktop repo;
//   - allowDefaultProject names this repo's untracked-by-tsconfig files;
//   - the JSX-only selectors went with the React code.
import globals from 'globals';
import tseslint from 'typescript-eslint';
import eslintPluginEslintComments from '@eslint-community/eslint-plugin-eslint-comments';
import eslintConfigPrettier from 'eslint-config-prettier';

const CONSOLE_RAW_ERROR = {
  selector:
    'CallExpression[callee.type="MemberExpression"][callee.object.name="console"][callee.property.name=/^(error|warn)$/] > Identifier:last-child:not(:first-child)',
  message:
    'Do not pass raw err/Error objects to console.error/warn — err.cause (ES2022) may propagate secrets (auth tickets, refresh tokens, response-body fragments). Use `(err as Error).message` or `error instanceof Error ? error.message : "unknown"` instead. If the raw argument is an audit-safe structured value, add `eslint-disable-next-line no-restricted-syntax -- <rationale>`.',
};

export default [
  {
    ignores: [
      '**/dist/',
      '**/node_modules/',
      '**/coverage/**', // Istanbul HTML report artifacts
      '**/*.js',
      '**/*.mjs',
      '**/*.mts',
    ],
  },

  ...tseslint.configs.recommended,

  // Prettier owns formatting; disable stylistic rules that would fight it.
  eslintConfigPrettier,

  {
    plugins: {
      '@eslint-community/eslint-comments': eslintPluginEslintComments,
    },

    languageOptions: {
      globals: {
        ...globals.browser,
        ...globals.es2022,
        ...globals.node,
      },
      parserOptions: {
        ecmaVersion: 'latest',
        sourceType: 'module',
        projectService: {
          allowDefaultProject: ['vitest.config.ts', 'packages/*/vitest.config.ts'],
        },
        tsconfigRootDir: import.meta.dirname,
      },
    },

    rules: {
      '@typescript-eslint/no-unused-vars': [
        'error',
        {
          argsIgnorePattern: '^_',
          varsIgnorePattern: '^_',
          destructuredArrayIgnorePattern: '^_',
        },
      ],
      '@typescript-eslint/no-explicit-any': 'error',
      '@typescript-eslint/no-empty-function': 'off',
      '@typescript-eslint/no-non-null-assertion': 'error',

      'no-console': ['warn', { allow: ['warn', 'error', 'debug'] }],
      'no-debugger': 'error',
      'no-duplicate-imports': 'error',
      'prefer-const': 'warn',
      eqeqeq: ['error', 'always', { null: 'ignore' }],

      // Every eslint-disable must say why.
      '@eslint-community/eslint-comments/require-description': ['error', { ignore: [] }],
    },
  },

  {
    files: ['packages/*/src/**/*.ts'],
    rules: {
      'no-restricted-syntax': [
        'error',
        CONSOLE_RAW_ERROR,
        {
          selector:
            'CallExpression[callee.object.name="console"] ' +
            'Identifier[name=/^(iceServers|pipIceServers|getIceServersForPip|getPipSession|pipSessionChannelName)$/]' +
            ':not(CallExpression[callee.name="describeIceServers"] *)',
          message:
            'NC-1: do not pass the ICE-server list — or anything derived from it — to console.*. Entries carry live HMAC TURN credentials and every console method is captured into the bug-report buffer that reaches a PUBLIC repo. Log describeIceServers(list) instead, which emits scheme and count only.',
        },
      ],
    },
  },

  {
    files: ['**/tests/**', '**/*.test.ts', '**/*.spec.ts'],
    rules: {
      '@typescript-eslint/no-explicit-any': 'off',
      '@typescript-eslint/no-non-null-assertion': 'off',
      '@typescript-eslint/no-unused-vars': 'off',
      '@eslint-community/eslint-comments/require-description': 'off',
    },
  },
];
