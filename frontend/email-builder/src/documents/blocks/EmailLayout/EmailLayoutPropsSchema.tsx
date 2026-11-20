import { z } from 'zod';

const COLOR_SCHEMA = z
  .string()
  .regex(/^#[0-9a-fA-F]{6}$/)
  .nullable()
  .optional();

const FONT_FAMILY_SCHEMA = z
  .enum([
    'MODERN_SANS',
    'BOOK_SANS',
    'ORGANIC_SANS',
    'GEOMETRIC_SANS',
    'HEAVY_SANS',
    'ROUNDED_SANS',
    'MODERN_SERIF',
    'BOOK_SERIF',
    'MONOSPACE',
  ])
  .nullable()
  .optional();

const EmailLayoutPropsSchema = z.object({
  backdropColor: COLOR_SCHEMA,
  borderColor: COLOR_SCHEMA,
  borderRadius: z.number().optional().nullable(),
  // Max width (px) of the content canvas. Defaults to DEFAULT_CANVAS_MAX_WIDTH.
  maxWidth: z.number().optional().nullable(),
  canvasColor: COLOR_SCHEMA,
  textColor: COLOR_SCHEMA,
  fontFamily: FONT_FAMILY_SCHEMA,
  childrenIds: z.array(z.string()).optional().nullable(),
  outlook: z.boolean().optional().nullable(),
  // Element (h1, p, ul...) -> inline CSS declarations applied to rendered Markdown.
  markdownStyles: z.record(z.string(), z.string()).optional().nullable(),
});

// The upstream @usewaypoint/email-builder renderer hardcodes this canvas width.
export const DEFAULT_CANVAS_MAX_WIDTH = 600;

export default EmailLayoutPropsSchema;

export type EmailLayoutProps = z.infer<typeof EmailLayoutPropsSchema>;
