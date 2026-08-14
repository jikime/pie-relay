import * as React from 'react'

import { cn } from '@/lib/utils'

function Input({ className, type, ...props }: React.ComponentProps<'input'>) {
  return <input data-slot="input" type={type} className={cn(className)} {...props} />
}

export { Input }
