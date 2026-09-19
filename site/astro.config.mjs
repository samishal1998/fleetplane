// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import starlightLinksValidator from 'starlight-links-validator';
import starlightOpenAPI, { createOpenAPISidebarGroup } from 'starlight-openapi';

// The generated OpenAPI pages land in this sidebar group.
const apiReferenceGroup = createOpenAPISidebarGroup();

export default defineConfig({
	site: 'https://samishal1998.github.io',
	base: '/fleetplane',
	integrations: [
		starlight({
			title: 'Fleetplane',
			description: 'The agentless control plane for VM fleets.',
			logo: {
				light: './src/assets/logo-light.svg',
				dark: './src/assets/logo-dark.svg',
				replacesTitle: true,
			},
			favicon: '/favicon.svg',
			social: [
				{ icon: 'github', label: 'GitHub', href: 'https://github.com/samishal1998/fleetplane' },
			],
			editLink: {
				baseUrl: 'https://github.com/samishal1998/fleetplane/edit/main/site/',
			},
			customCss: ['./src/styles/theme.css'],
			// Code surfaces are always dark, in both themes — as on the landing page.
			expressiveCode: { themes: ['github-dark'] },
			plugins: [
				starlightLinksValidator({
					// The OpenAPI pages are injected routes the validator cannot see;
					// the quickstart links the local dashboard on purpose.
					exclude: ['/fleetplane/api-reference/', '/fleetplane/api-reference/**'],
					errorOnLocalLinks: false,
				}),
				starlightOpenAPI([
					{
						base: 'api-reference',
						schema: '../api/openapi.yaml',
						sidebar: {
							label: 'Operations',
							collapsed: false,
							group: apiReferenceGroup,
							operations: { labels: 'path', badges: true },
						},
					},
				]),
			],
			sidebar: [
				{
					label: 'Usage guides',
					items: [
						{
							label: 'Start here',
							items: [
								'guides/introduction',
								'guides/concepts',
								'guides/installation',
								'guides/quickstart',
								'guides/configuration',
							],
						},
						{
							label: 'Guides',
							items: [
								'guides/pools',
								'guides/acquiring',
								'guides/parked-machines',
								'guides/dashboard',
							],
						},
						{
							label: 'Providers',
							items: [
								{ label: 'Overview', slug: 'guides/providers/overview' },
								'guides/providers/hetzner',
								'guides/providers/digitalocean',
								'guides/providers/aws',
								'guides/providers/gcp',
								'guides/providers/docker',
								'guides/providers/fake',
							],
						},
						{
							label: 'Operations',
							items: [
								'guides/operations/production',
								'guides/operations/metrics',
								'guides/operations/backup',
								'guides/operations/uncertain-operations',
								'guides/operations/tokens',
							],
						},
						{
							label: 'Reference',
							items: ['reference/cli', 'reference/http-api'],
						},
					],
				},
				{
					label: 'Developer guides',
					items: [
						'developers/architecture',
						'developers/repo-layout',
						'developers/building',
						'developers/testing',
						'developers/provider-sdk',
						{
							label: 'Decision records',
							collapsed: true,
							items: [
								{ label: 'Index', slug: 'developers/adr' },
								{ autogenerate: { directory: 'developers/adr' } },
							],
						},
					],
				},
				{
					label: 'HTTP API reference',
					items: [
						{ label: 'HTTP API guide', slug: 'reference/http-api' },
						apiReferenceGroup,
					],
				},
			],
		}),
	],
});
