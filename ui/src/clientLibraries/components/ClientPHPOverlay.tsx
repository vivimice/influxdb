// Libraries
import React, {FunctionComponent} from 'react'
import {connect} from 'react-redux'

// Components
import ClientLibraryOverlay from 'src/clientLibraries/components/ClientLibraryOverlay'
import TemplatedCodeSnippet from 'src/shared/components/TemplatedCodeSnippet'

// Constants
import {clientPHPLibrary} from 'src/clientLibraries/constants'

// Selectors
import {getOrg} from 'src/organizations/selectors'

// Types
import {AppState} from 'src/types'

interface StateProps {
  org: string
}

type Props = StateProps

const ClientPHPOverlay: FunctionComponent<Props> = props => {
  const {
    name,
    url,
    initializeComposerCodeSnippet,
    initializeClientCodeSnippet,
    executeQueryCodeSnippet,
    writingDataLineProtocolCodeSnippet,
    writingDataPointCodeSnippet,
    writingDataArrayCodeSnippet,
  } = clientPHPLibrary
  const {org} = props
  const server = window.location.origin

  return (
    <ClientLibraryOverlay title={`${name} Client Library`}>
      <p>
        For more detailed and up to date information check out the{' '}
        <a href={url} target="_blank">
          GitHub Repository
        </a>
      </p>
      <h5>Install via Composer</h5>
      <TemplatedCodeSnippet
        template={initializeComposerCodeSnippet}
        label="Code"
      />
      <h5>Initialize the Client</h5>
      <TemplatedCodeSnippet
        template={initializeClientCodeSnippet}
        label="PHP Code"
        defaults={{
          server: 'basepath',
          token: 'token',
          org: 'orgID',
          bucket: 'bucketID',
        }}
        values={{
          server,
          org,
        }}
      />
      <h5>Write Data</h5>
      <p>Option 1: Use InfluxDB Line Protocol to write data</p>
      <TemplatedCodeSnippet
        template={writingDataLineProtocolCodeSnippet}
        label="PHP Code"
      />
      <p>Option 2: Use a Data Point to write data</p>
      <TemplatedCodeSnippet
        template={writingDataPointCodeSnippet}
        label="PHP Code"
      />
      <p>Option 3: Use an Array structure to write data</p>
      <TemplatedCodeSnippet
        template={writingDataArrayCodeSnippet}
        label="PHP Code"
      />
      <h5>Execute a Flux query</h5>
      <TemplatedCodeSnippet
        template={executeQueryCodeSnippet}
        label="PHP Code"
      />
    </ClientLibraryOverlay>
  )
}

const mstp = (state: AppState) => {
  const {id} = getOrg(state)

  return {
    org: id,
  }
}

export {ClientPHPOverlay}
export default connect<StateProps, {}, Props>(mstp, null)(ClientPHPOverlay)
