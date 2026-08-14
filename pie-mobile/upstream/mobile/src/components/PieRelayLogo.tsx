import { Image } from 'react-native'

type Props = {
  size?: number
}

export function PieRelayLogo({ size = 24 }: Props) {
  return (
    <Image
      source={require('../../assets/pielab-mark-ondark.png')}
      style={{ width: size, height: size }}
      resizeMode="contain"
      accessibilityLabel="Pie Relay"
    />
  )
}
