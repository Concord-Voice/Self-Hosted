import winston from 'winston';
import type { Logform } from 'winston';
import { config } from '../config/index.js';
import { boundedFields } from './logBounds.js';

/**
 * The log format chain. `boundedFields()` sits after `errors()` (so an Error's
 * message and stack are bounded too) and before the finalizing format, so the
 * written line is built only from bounded values (VULN-157).
 */
export function createLogFormat(environment: string): Logform.Format {
  return winston.format.combine(
    winston.format.timestamp(),
    winston.format.errors({ stack: true }),
    boundedFields(),
    environment === 'production'
      ? winston.format.json()
      : winston.format.combine(
          winston.format.colorize(),
          winston.format.printf(({ level, message, timestamp, ...meta }) => {
            const metaStr = Object.keys(meta).length ? JSON.stringify(meta, null, 2) : '';
            return `${timestamp} [${level}]: ${message} ${metaStr}`;
          })
        )
  );
}

export const logger = winston.createLogger({
  level: process.env.LOG_LEVEL || (config.environment === 'production' ? 'info' : 'debug'),
  format: createLogFormat(config.environment),
  transports: [
    new winston.transports.Console(),
  ],
});
