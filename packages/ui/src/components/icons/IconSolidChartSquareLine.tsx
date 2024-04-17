import React from 'react'

import { IconProps } from './types'

export const IconSolidChartSquareLine = ({
	size = '1em',
	...props
}: IconProps) => {
	return (
		<svg
			xmlns="http://www.w3.org/2000/svg"
			width={size}
			height={size}
			fill="none"
			viewBox="0 0 16 16"
			focusable="false"
			{...props}
		>
			<path
				id="icon"
				fillRule="evenodd"
				clipRule="evenodd"
				d="M2.40039 3.9999C2.40039 3.11625 3.11674 2.3999 4.00039 2.3999H12.0004C12.884 2.3999 13.6004 3.11625 13.6004 3.9999V11.9999C13.6004 12.8836 12.884 13.5999 12.0004 13.5999H4.00039C3.11674 13.5999 2.40039 12.8836 2.40039 11.9999V3.9999ZM11.7661 5.76559C12.0785 6.07801 12.0785 6.58454 11.7661 6.89696L8.56608 10.097C8.25366 10.4094 7.74712 10.4094 7.43471 10.097L6.40039 9.06264L5.36608 10.097C5.05366 10.4094 4.54712 10.4094 4.23471 10.097C3.92229 9.78454 3.92229 9.27801 4.23471 8.96559L5.83471 7.36559C6.14712 7.05317 6.65366 7.05317 6.96608 7.36559L8.00039 8.3999L10.6347 5.76559C10.9471 5.45317 11.4537 5.45317 11.7661 5.76559Z"
				fill="currentColor"
			/>
		</svg>
	)
}