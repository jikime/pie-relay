import * as React from 'react'
import { cva, type VariantProps } from 'class-variance-authority'

import { cn } from '@/lib/utils'

const buttonVariants = cva('button', {
  variants: {
    variant: {
      primary: 'primary',
      secondary: 'secondary',
      ghost: 'ghost',
    },
  },
  defaultVariants: { variant: 'primary' },
})

function Button({
  className,
  variant,
  type = 'button',
  ...props
}: React.ComponentProps<'button'> & VariantProps<typeof buttonVariants>) {
  return (
    <button
      data-slot="button"
      type={type}
      className={cn(buttonVariants({ variant }), className)}
      {...props}
    />
  )
}

export { Button, buttonVariants }
